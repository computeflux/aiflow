import { useState } from "react";
import { Loader2, Plus, Edit3, Trash2, Check, Server, Send, ShieldCheck, Mail } from "lucide-react";
import { fetchEmailSenders, createEmailSender, updateEmailSender, deleteEmailSender, testEmailSender, type EmailSender } from "../api";
import { useAsync } from "../hooks";
import { Link } from "react-router-dom";

const PROVIDER_PRESETS: Array<{
  name: string;
  host: string;
  port: number;
  note: string;
}> = [
  { name: "Resend SMTP", host: "smtp.resend.com", port: 465, note: "用户名为 resend，密码为 API Key" },
  { name: "阿里云邮件推送 (DirectMail)", host: "smtpdm.aliyun.com", port: 465, note: "DirectMail 专用 SMTP 发信" },
  { name: "腾讯企业邮 / Foxmail", host: "smtp.exmail.qq.com", port: 465, note: "需在企业微信/邮箱中生成客户端专用密码" },
  { name: "QQ 个人邮箱", host: "smtp.qq.com", port: 465, note: "需要在设置中开启 POP3/SMTP 并生成授权码" },
  { name: "网易 163 邮箱", host: "smtp.163.com", port: 465, note: "需使用客户端专用授权密码" },
  { name: "Gmail SMTP", host: "smtp.gmail.com", port: 587, note: "需开启两步验证并使用 App Password" },
  { name: "SendGrid", host: "smtp.sendgrid.net", port: 587, note: "用户名为 apikey，密码为 API Key" },
  { name: "Amazon SES (us-east-1)", host: "email-smtp.us-east-1.amazonaws.com", port: 465, note: "使用 SES 生成的 SMTP 用户名和密码" },
];

export function EmailSenders() {
  const [success, setSuccess] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [editingSender, setEditingSender] = useState<EmailSender | null>(null);
  const [actionId, setActionId] = useState<number | null>(null);
  const [testingId, setTestingId] = useState<number | null>(null);
  const [testEmailAddr, setTestEmailAddr] = useState("");
  const [testModalSender, setTestModalSender] = useState<EmailSender | null>(null);

  // Form fields
  const [name, setName] = useState("");
  const [host, setHost] = useState("");
  const [port, setPort] = useState(465);
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [fromAddress, setFromAddress] = useState("");
  const [fromName, setFromName] = useState("");
  const [dailyLimit, setDailyLimit] = useState(0);
  const [enabled, setEnabled] = useState(true);
  const [isDefault, setIsDefault] = useState(false);

  const { data: senders, loading, error, reload: load, setError } = useAsync<EmailSender[]>(async () => (await fetchEmailSenders()).senders, []);

  const resetForm = () => {
    setShowForm(false);
    setEditingSender(null);
    setName("");
    setHost("");
    setPort(465);
    setUser("");
    setPassword("");
    setFromAddress("");
    setFromName("");
    setDailyLimit(0);
    setEnabled(true);
    setIsDefault(false);
  };

  const applyPreset = (preset: typeof PROVIDER_PRESETS[0]) => {
    setName(preset.name);
    setHost(preset.host);
    setPort(preset.port);
  };

  const handleEdit = (s: EmailSender) => {
    setEditingSender(s);
    setName(s.name);
    setHost(s.host);
    setPort(s.port);
    setUser(s.user);
    setPassword(""); // keep blank if unchanged
    setFromAddress(s.from_address);
    setFromName(s.from_name || "");
    setDailyLimit(s.daily_limit || 0);
    setEnabled(s.enabled);
    setIsDefault(s.is_default);
    setShowForm(true);
  };

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim() || !host.trim() || !user.trim() || !fromAddress.trim()) {
      setError("请填写完整的服务商名称、Host、用户名和发件人邮箱");
      return;
    }
    if (!editingSender && !password.trim()) {
      setError("新建发信邮箱时密码/授权码必填");
      return;
    }
    setError(null);
    setSuccess(null);
    const data: Parameters<typeof createEmailSender>[0] = {
      name: name.trim(),
      host: host.trim(),
      port: Number(port) || 465,
      user: user.trim(),
      password: password.trim() || undefined,
      from_address: fromAddress.trim(),
      from_name: fromName.trim(),
      daily_limit: Number(dailyLimit) || 0,
      enabled,
      is_default: isDefault,
    };
    try {
      if (editingSender) {
        await updateEmailSender(editingSender.id, data);
        setSuccess(`发信配置 "${name}" 更新成功`);
      } else {
        await createEmailSender(data);
        setSuccess(`发信配置 "${name}" 添加成功`);
      }
      resetForm();
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleDelete = async (s: EmailSender) => {
    if (!window.confirm(`确认删除发信服务商 "${s.name}" (${s.from_address}) 吗？`)) return;
    setActionId(s.id);
    setError(null);
    try {
      await deleteEmailSender(s.id);
      setSuccess(`已删除 "${s.name}"`);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setActionId(null);
    }
  };

  const handleTestSend = async () => {
    if (!testModalSender) return;
    setTestingId(testModalSender.id);
    setError(null);
    setSuccess(null);
    try {
      const res = await testEmailSender(testModalSender.id, testEmailAddr.trim());
      setSuccess(res.message || "测试邮件发送成功！");
      setTestModalSender(null);
      setTestEmailAddr("");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setTestingId(null);
    }
  };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase flex items-center gap-2">
            <Server className="h-6 w-6" /> 发信邮箱与服务商管理
          </h1>
          <p className="mt-1 text-sm text-(--text-muted)">
            在数据库中动态配置多个 SMTP 发信邮箱 / 邮件服务商（Resend、阿里云、腾讯企邮、SES 等），支持发信隔离与多账户轮转
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Link
            to="/email/"
            className="border border-(--border-subtle) px-3 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition flex items-center gap-2"
          >
            <Mail className="h-4 w-4" /> 返回邮件推广
          </Link>
          <button
            onClick={() => {
              if (showForm) resetForm();
              else setShowForm(true);
            }}
            className="border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) transition flex items-center gap-2"
          >
            <Plus className="h-4 w-4" />
            {showForm ? "取消" : "添加发信服务商"}
          </button>
        </div>
      </header>

      {error && <div className="mb-4 border border-(--danger)/50 bg-(--danger)/10 p-3 text-sm text-(--danger)">{error}</div>}
      {success && <div className="mb-4 border border-(--success)/50 bg-(--success)/10 p-3 text-sm text-(--success)">{success}</div>}

      {showForm && (
        <form onSubmit={handleSave} className="mb-8 border border-(--border-subtle) p-4 space-y-4 bg-(--bg-card)">
          <div className="flex items-center justify-between">
            <h3 className="text-xs font-bold uppercase text-(--text-secondary)">
              {editingSender ? `编辑发信配置 #${editingSender.id}` : "添加发信邮箱 / 服务商"}
            </h3>
            {!editingSender && (
              <div className="flex items-center gap-2 text-xs text-(--text-muted)">
                <span>快捷预设：</span>
                <div className="flex flex-wrap gap-1.5">
                  {PROVIDER_PRESETS.map((p, idx) => (
                    <button
                      key={idx}
                      type="button"
                      onClick={() => applyPreset(p)}
                      className="border border-(--border-subtle) px-2 py-0.5 text-[11px] hover:bg-(--bg-elevated) transition"
                      title={p.note}
                    >
                      {p.name.split(" ")[0]}
                    </button>
                  ))}
                </div>
              </div>
            )}
          </div>

          <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">配置别名 / 服务商名称</label>
              <input
                value={name}
                onChange={e => setName(e.target.value)}
                placeholder="如: 阿里云 DirectMail 或 Resend-主号"
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
              />
            </div>
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">SMTP Host</label>
              <input
                value={host}
                onChange={e => setHost(e.target.value)}
                placeholder="smtp.resend.com"
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
              />
            </div>
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">SMTP 端口 (465 SSL / 587 STARTTLS)</label>
              <input
                type="number"
                value={port}
                onChange={e => setPort(Number(e.target.value))}
                placeholder="465"
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
              />
            </div>
          </div>

          <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">SMTP 用户名 / 账号 / API Key</label>
              <input
                value={user}
                onChange={e => setUser(e.target.value)}
                placeholder="username 或 resend / apikey"
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white font-mono"
              />
            </div>
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">
                密码 / 授权码 / 密钥 {editingSender && <span className="text-(--text-muted) font-normal">(留空则不修改)</span>}
              </label>
              <input
                type="password"
                value={password}
                onChange={e => setPassword(e.target.value)}
                placeholder={editingSender ? "********" : "SMTP 密码或 API 密钥"}
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white font-mono"
              />
            </div>
          </div>

          <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">发件人邮箱 (From Address)</label>
              <input
                value={fromAddress}
                onChange={e => setFromAddress(e.target.value)}
                placeholder="contact@domain.com"
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white font-mono"
              />
            </div>
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">发件人显示名称 (From Name)</label>
              <input
                value={fromName}
                onChange={e => setFromName(e.target.value)}
                placeholder="ComputeFlux"
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
              />
            </div>
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">每日发信上限 (0 为不限制)</label>
              <input
                type="number"
                value={dailyLimit}
                onChange={e => setDailyLimit(Number(e.target.value))}
                placeholder="0"
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"
              />
            </div>
          </div>

          <div className="flex items-center gap-6 pt-2">
            <label className="flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={enabled}
                onChange={e => setEnabled(e.target.checked)}
                className="accent-white"
              />
              <span className="text-xs text-(--text-secondary)">启用该发信服务商</span>
            </label>
            <label className="flex items-center gap-2 cursor-pointer">
              <input
                type="checkbox"
                checked={isDefault}
                onChange={e => setIsDefault(e.target.checked)}
                className="accent-white"
              />
              <span className="text-xs text-(--text-secondary)">设为默认发信邮箱 (批次未指定时优先使用)</span>
            </label>
          </div>

          <div className="flex gap-2 pt-2">
            <button
              type="submit"
              className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm hover:bg-(--text-primary) transition"
            >
              {editingSender ? "保存修改" : "确认添加"}
            </button>
            <button
              type="button"
              onClick={resetForm}
              className="border border-(--border-subtle) text-(--text-secondary) px-4 py-1.5 text-sm hover:bg-(--bg-elevated) transition"
            >
              取消
            </button>
          </div>
        </form>
      )}

      <div className="border border-(--border-subtle)">
        <table className="w-full text-left text-sm">
          <thead className="border-b border-(--border-subtle) text-(--text-muted)">
            <tr>
              <th className="px-4 py-3 font-bold uppercase w-12">#</th>
              <th className="px-4 py-3 font-bold uppercase">别名 / 服务商</th>
              <th className="px-4 py-3 font-bold uppercase">Host & 端口</th>
              <th className="px-4 py-3 font-bold uppercase">发件人 / 用户名</th>
              <th className="px-4 py-3 font-bold uppercase w-28 text-right">今日发送</th>
              <th className="px-4 py-3 font-bold uppercase w-20 text-center">状态</th>
              <th className="px-4 py-3 font-bold uppercase w-20 text-center">默认</th>
              <th className="px-4 py-3 text-right font-bold uppercase w-36">操作</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-(--border-divider)">
            {loading && (senders?.length ?? 0) === 0 ? (
              <tr>
                <td colSpan={8} className="px-4 py-8 text-center text-(--text-muted)">
                  <Loader2 className="h-5 w-5 animate-spin mx-auto" />
                </td>
              </tr>
            ) : (senders?.length ?? 0) === 0 ? (
              <tr>
                <td colSpan={8} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">
                  暂无数据库发信邮箱配置（目前退回到环境变量 SMTP）。点击右上角「添加发信服务商」创建。
                </td>
              </tr>
            ) : (
              (senders ?? []).map(s => (
                <tr key={s.id} className="hover:bg-(--bg-elevated) transition">
                  <td className="px-4 py-3 font-mono text-xs text-(--text-muted)">{s.id}</td>
                  <td className="px-4 py-3 font-bold text-(--text-primary)">
                    <div className="flex items-center gap-1.5">
                      <span>{s.name}</span>
                      {s.from_name && <span className="text-xs text-(--text-muted)">({s.from_name})</span>}
                    </div>
                  </td>
                  <td className="px-4 py-3 font-mono text-xs text-(--text-secondary)">
                    {s.host}:{s.port}
                  </td>
                  <td className="px-4 py-3 text-xs">
                    <div className="font-mono text-(--text-primary)">{s.from_address}</div>
                    <div className="text-(--text-muted)">user: {s.user}</div>
                  </td>
                  <td className="px-4 py-3 text-right font-mono text-xs">
                    {s.used_today}
                    {s.daily_limit > 0 ? <span className="text-(--text-muted)">/{s.daily_limit}</span> : ""}
                  </td>
                  <td className="px-4 py-3 text-center">
                    {s.enabled ? (
                      <span className="text-xs text-(--success) font-bold">启用</span>
                    ) : (
                      <span className="text-xs text-(--text-muted)">禁用</span>
                    )}
                  </td>
                  <td className="px-4 py-3 text-center">
                    {s.is_default && (
                      <span className="inline-flex items-center gap-1 text-xs text-(--accent) font-bold">
                        <Check className="h-3.5 w-3.5" /> 默认
                      </span>
                    )}
                  </td>
                  <td className="px-4 py-3 text-right">
                    <div className="flex items-center justify-end gap-1">
                      <button
                        onClick={() => {
                          setTestModalSender(s);
                          setTestEmailAddr(s.from_address);
                        }}
                        title="测试发信连通性"
                        className="p-1.5 border border-(--border-subtle) text-(--text-secondary) hover:text-(--accent) hover:bg-(--bg-hover) transition"
                      >
                        <Send className="h-3.5 w-3.5" />
                      </button>
                      <button
                        onClick={() => handleEdit(s)}
                        title="编辑"
                        className="p-1.5 border border-(--border-subtle) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover) transition"
                      >
                        <Edit3 className="h-3.5 w-3.5" />
                      </button>
                      <button
                        onClick={() => handleDelete(s)}
                        disabled={actionId === s.id}
                        title="删除"
                        className="p-1.5 border border-(--border-subtle) text-(--danger) hover:bg-(--danger)/20 transition disabled:opacity-50"
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </button>
                    </div>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {/* Test Email Modal */}
      {testModalSender && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
          onClick={() => setTestModalSender(null)}
        >
          <div
            className="bg-(--bg-app) border border-(--border-subtle) w-full max-w-md p-6 space-y-4"
            onClick={e => e.stopPropagation()}
          >
            <h3 className="text-base font-bold flex items-center gap-2">
              <ShieldCheck className="h-5 w-5 text-(--success)" /> 测试发信连通性
            </h3>
            <p className="text-xs text-(--text-muted)">
              将使用服务商 <strong>{testModalSender.name}</strong> ({testModalSender.host}:{testModalSender.port}) 发送一封测试邮件。
            </p>
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">测试接收邮箱</label>
              <input
                value={testEmailAddr}
                onChange={e => setTestEmailAddr(e.target.value)}
                placeholder="your-email@example.com"
                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white font-mono"
              />
            </div>
            <div className="flex justify-end gap-2 pt-2">
              <button
                type="button"
                onClick={() => setTestModalSender(null)}
                className="border border-(--border-subtle) text-(--text-secondary) px-4 py-1.5 text-sm hover:bg-(--bg-elevated)"
              >
                取消
              </button>
              <button
                type="button"
                onClick={handleTestSend}
                disabled={testingId === testModalSender.id || !testEmailAddr.trim()}
                className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm hover:bg-(--text-primary) transition disabled:opacity-50 flex items-center gap-2"
              >
                {testingId === testModalSender.id ? (
                  <Loader2 className="h-4 w-4 animate-spin" />
                ) : (
                  <Send className="h-4 w-4" />
                )}
                立即发送测试
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
