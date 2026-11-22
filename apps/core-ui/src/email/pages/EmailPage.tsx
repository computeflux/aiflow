import React, { useRef, useState } from "react";
import { Loader2, RefreshCw, Plus, ChevronDown, ChevronUp, Mail, FileText, Clock, Send, X, Server } from "lucide-react";
import { Link, useNavigate } from "react-router-dom";
import { RichEditor } from "@kit";
import { useAsync, useInterval } from "../hooks";
import { startEmailWorkflow, fetchEmailBatches, fetchEmailBatch, sendEmailBatch, rejectEmailBatch, deleteEmailBatch, fetchEmailTemplates, fetchEmailSendStats, fetchEmailSenders, fetchGitHubOrgs, type EmailBatch, type EmailRecipient, type EmailTemplate, type EmailRecipientStat, type EmailSender } from "../api";
import { fmtTime } from "../utils";

const statusLabels: Record<string, string> = { analyzing: "分析中", pending: "待审批", sending: "发送中", sent: "已发送", failed: "失败", rejected: "已拒绝" };
const statusColors: Record<string, string> = { analyzing: "text-(--purple)", pending: "text-(--warning)", sending: "text-(--purple)", sent: "text-(--success)", failed: "text-(--danger)", rejected: "text-(--text-muted)" };

export function EmailPage() {
  const [success, setSuccess] = useState<string | null>(null);
  const [orgName, setOrgName] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const navigate = useNavigate();
  const [expandedId, setExpandedId] = useState<number | null>(null);
  const [detail, setDetail] = useState<{ batch: EmailBatch; recipients: EmailRecipient[] } | null>(null);
  const [loadingDetail, setLoadingDetail] = useState(false);
  const [actionId, setActionId] = useState<number | null>(null);

  // Send modal state
  const [sendModal, setSendModal] = useState<EmailBatch | null>(null);
  const [sendStats, setSendStats] = useState<EmailRecipientStat[]>([]);
  const [sendSubject, setSendSubject] = useState("");
  const [sendContent, setSendContent] = useState("");
  const [sendCc, setSendCc] = useState("");
  const [sendSenderId, setSendSenderId] = useState<number | undefined>(undefined);
  const [_templates, setTemplates] = useState<EmailTemplate[]>([]);
  const [senders, setSenders] = useState<EmailSender[]>([]);
  const [loadingSend, setLoadingSend] = useState(false);

  const { data, loading, error, reload: load, reloadSilently, setError } = useAsync<EmailBatch[]>(
    async () => (await fetchEmailBatches()).batches,
    [],
  );
  const batches = data ?? [];

  // 组织列表加载失败不影响主流程（静默：失败不弹错、返回空列表）
  const { data: orgData, loading: loadingOrgs } = useAsync<string[]>(
    async () => {
      try { return await fetchGitHubOrgs(); } catch { return []; }
    },
    [],
  );
  const githubOrgs = orgData ?? [];

  // analyzing 状态下 2s 静默轮询（不闪 loading、失败不清列表）
  const hasAnalyzing = batches.some(b => b.status === "analyzing");
  const pollBusyRef = useRef(false);
  useInterval(
    () => {
      if (pollBusyRef.current) return; // skip if previous poll still in flight
      pollBusyRef.current = true;
      reloadSilently().finally(() => { pollBusyRef.current = false; });
    },
    hasAnalyzing ? 2000 : null,
  );

  const handleStart = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!orgName.trim()) return;
    setSubmitting(true); setSuccess(null);
    try {
      await startEmailWorkflow(orgName.trim());
      setOrgName("");
      setSuccess(`已提交 "${orgName.trim()}" 的分析任务，正在后台分析中...`);
      await load();
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setSubmitting(false); }
  };

  const toggleDetail = async (id: number) => {
    if (expandedId === id) { setExpandedId(null); setDetail(null); return; }
    setLoadingDetail(true);
    try { setDetail(await fetchEmailBatch(id)); setExpandedId(id); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setLoadingDetail(false); }
  };

  const handleReject = async (id: number) => {
    setActionId(id);
    try { await rejectEmailBatch(id); await load(); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setActionId(null); }
  };

  const handleDelete = async (b: EmailBatch) => {
    if (!window.confirm(`确认删除批次 #${b.id}（${b.org_name}）？该操作会连同收件人一并移除，不可恢复。`)) return;
    setActionId(b.id);
    try {
      await deleteEmailBatch(b.id);
      if (expandedId === b.id) { setExpandedId(null); setDetail(null); }
      setSuccess(`已删除批次 #${b.id}`);
      await load();
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setActionId(null); }
  };

  // Open custom send modal
  const openSendModal = async (batch: EmailBatch) => {
    setSendModal(batch);
    setSendCc(batch.cc_list || "");
    setSendSenderId(batch.email_sender_id);
    setLoadingSend(true);
    try {
      const [statsRes, tplsRes, sendersRes] = await Promise.allSettled([
        fetchEmailSendStats(batch.id),
        fetchEmailTemplates(),
        fetchEmailSenders(),
      ]);
      let templates: EmailTemplate[] = [];
      if (statsRes.status === "fulfilled") {
        setSendStats(statsRes.value.recipients.map(s => ({ ...s, selected: s.status !== "sent" && s.status !== "skipped" })));
      } else {
        setError(statsRes.reason instanceof Error ? statsRes.reason.message : String(statsRes.reason));
      }
      if (tplsRes.status === "fulfilled") {
        templates = tplsRes.value.templates;
        setTemplates(templates);
      } else {
        setError(tplsRes.reason instanceof Error ? tplsRes.reason.message : String(tplsRes.reason));
      }
      if (sendersRes.status === "fulfilled") {
        const list = sendersRes.value.senders;
        setSenders(list);
      } else {
        setError(sendersRes.reason instanceof Error ? sendersRes.reason.message : String(sendersRes.reason));
      }
      // 预填模板：优先本批次上次发送保存的主题/内容 → 其次复用同组织最近一次已发送批次留下的模板 → 最后回退默认模板
      const lastSentSameOrg = batches
        .filter(x => x.org_name === batch.org_name && x.id !== batch.id && x.status === "sent" && x.custom_content)
        .sort((a, b) => b.id - a.id)[0];
      const subject = batch.custom_subject || lastSentSameOrg?.custom_subject || "";
      const content = batch.custom_content || lastSentSameOrg?.custom_content || "";
      const def = templates.find(t => t.is_default) || templates[0];
      setSendSubject(subject ? subject.replace(/{org_name}/g, batch.org_name) : (def ? (def.subject ?? "").replace(/{org_name}/g, batch.org_name) : ""));
      setSendContent(content ? content.replace(/{org_name}/g, batch.org_name) : (def ? (def.body ?? "").replace(/{org_name}/g, batch.org_name) : ""));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally { setLoadingSend(false); }
  };

  const handleSendConfirm = async () => {
    if (!sendModal) return;
    const selectedIds = sendStats.filter(s => s.selected).map(s => s.id);
    if (selectedIds.length === 0) {
      setError("请至少选择一个收件人");
      return;
    }
    setActionId(sendModal.id);
    try {
      await sendEmailBatch(sendModal.id, {
        content: sendContent,
        subject: sendSubject,
        cc_list: sendCc,
        recipient_ids: selectedIds,
        email_sender_id: sendSenderId,
      });
      setSendModal(null);
      await load();
      if (expandedId === sendModal.id) toggleDetail(sendModal.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally { setActionId(null); }
  };

  const toggleGroup = (isSentGroup: boolean) => {
    const group = sendStats.filter(s => (s.status === "sent") === isSentGroup);
    const allSelected = group.length > 0 && group.every(s => s.selected);
    setSendStats(prev => prev.map(s => (s.status === "sent") === isSentGroup ? { ...s, selected: !allSelected } : s));
  };

  const toggleRecipient = (id: number) => {
    setSendStats(prev => prev.map(s => s.id === id ? { ...s, selected: !s.selected } : s));
  };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="mb-8 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase flex items-center gap-2"><Mail className="h-6 w-6" /> 邮件推广</h1>
          <p className="mt-1 text-sm text-(--text-muted)">分析 GitHub 组织开发者，生成 AI Token 推广邮件批次</p>
        </div>
        <div className="flex items-center gap-2">
          <Link to="/email/email-senders" className="border border-(--border-subtle) px-3 py-2 text-sm font-bold hover:bg-(--bg-elevated) transition flex items-center gap-2"><Server className="h-4 w-4" />发信服务商</Link>
          <Link to="/email/email-templates" className="border border-(--border-subtle) px-3 py-2 text-sm font-bold hover:bg-(--bg-elevated) transition flex items-center gap-2"><FileText className="h-4 w-4" />邮件模板</Link>
          <button onClick={() => navigate(`/email/ingest-tasks?new=1&source=github&keyword=${encodeURIComponent(orgName.trim())}`)} className="border border-(--border-subtle) px-3 py-2 text-sm font-bold hover:bg-(--bg-elevated) transition flex items-center gap-2"><Clock className="h-4 w-4" />定时抓取</button>
          <button onClick={load} disabled={loading} className="border border-(--border-subtle) px-4 py-2 text-sm font-bold hover:bg-(--bg-elevated) transition disabled:opacity-50 flex items-center gap-2">{loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />} 刷新</button>
        </div>
      </header>

      {error && <div className="mb-4 border border-(--danger)/50 bg-(--danger)/10 p-3 text-sm text-(--danger)">{error}</div>}
      {success && <div className="mb-4 border border-(--purple)/50 bg-(--purple)/10 p-3 text-sm text-[#A78BFA] flex items-center gap-2"><Clock className="h-4 w-4" /> {success}</div>}

      <form onSubmit={handleStart} className="mb-8 border border-(--border-subtle) p-4">
        <h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted) mb-3">发起邮件工作流</h2>
        <div className="flex flex-wrap gap-3 items-end">
          <div className="flex-1 min-w-75"><label className="block text-[10px] uppercase text-(--text-muted) mb-1">GitHub 组织名</label><input value={orgName} onChange={e => setOrgName(e.target.value)} placeholder="例如 paritytech" required className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-4 py-2 text-sm focus:outline-none focus:border-white" /></div>
          <button type="submit" disabled={submitting} className="bg-(--accent) text-(--accent-foreground) font-bold px-6 py-2 text-sm hover:bg-(--text-primary) transition disabled:opacity-50 flex items-center gap-2">{submitting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Plus className="h-4 w-4" />} 开始分析</button>
        </div>
        <p className="mt-2 text-[10px] text-(--text-muted)">自动从数据库获取该组织的 GitHub commit 记录 → 程序过滤非真实邮箱 → 生成邮件列表 → 管理员审批后发送</p>
      </form>

      <div className="mb-8 border border-(--border-subtle) p-4">
        <div className="flex items-center gap-2 mb-3">
          <h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted)">GitHub 组织名（素材库已有数据）</h2>
          {loadingOrgs && <Loader2 className="h-3.5 w-3.5 animate-spin text-(--text-muted)" />}
        </div>
        {githubOrgs.length === 0 ? (
          <p className="text-xs text-(--text-muted) font-mono">暂无 github 类型的素材数据</p>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {githubOrgs.map((org) => (
              <button key={org} type="button" onClick={() => setOrgName(org)} className={orgName === org ? "border-white bg-(--accent) text-(--accent-foreground) px-2 py-0.5 text-xs font-bold transition" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated) px-2 py-0.5 text-xs font-bold transition"}>{org}</button>
            ))}
          </div>
        )}
      </div>

      <div className="border border-(--border-subtle)">
        <div className="p-4 border-b border-(--border-subtle)"><h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted)">邮件批次列表</h2></div>
        <table className="w-full text-left text-sm">
          <thead className="border-b border-(--border-subtle) text-(--text-muted)"><tr><th className="px-4 py-3 font-bold uppercase">ID</th><th className="px-4 py-3 font-bold uppercase">组织</th><th className="px-4 py-3 font-bold uppercase">状态</th><th className="px-4 py-3 font-bold uppercase">收件人</th><th className="px-4 py-3 font-bold uppercase">创建时间</th><th className="px-4 py-3 text-right font-bold uppercase">操作</th></tr></thead>
          <tbody className="divide-y divide-(--border-divider)">
            {batches.length === 0 ? <tr><td colSpan={6} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">暂无邮件批次</td></tr> : batches.map((b) => (
              <React.Fragment key={b.id}>
                <tr className="hover:bg-(--bg-elevated) transition-colors cursor-pointer" onClick={() => toggleDetail(b.id)}>
                  <td className="px-4 py-3 font-mono text-(--text-secondary)">{b.id}</td><td className="px-4 py-3 text-(--text-primary) font-bold">{b.org_name}</td>
                  <td className="px-4 py-3"><span className={`text-xs font-bold uppercase ${statusColors[b.status] || ""}`}>{b.status === "analyzing" ? <span className="flex items-center gap-1"><Loader2 className="h-3 w-3 animate-spin" />{statusLabels[b.status]}</span> : statusLabels[b.status] || b.status}</span></td>
                  <td className="px-4 py-3 text-(--text-secondary)">{b.total > 0 ? `${b.sent}/${b.total}` : "-"}</td>
                  <td className="px-4 py-3 text-(--text-muted) text-xs">{fmtTime(b.created_at)}</td>
                  <td className="px-4 py-3 text-right" onClick={e => e.stopPropagation()}>
                    <div className="flex items-center justify-end gap-1">
                      {(b.status === "pending" || b.status === "sending" || b.status === "sent") && <>
                        <button onClick={() => openSendModal(b)} disabled={actionId === b.id} className="px-2 py-1 border border-(--success) text-(--success) text-xs font-bold hover:bg-(--success) hover:text-black flex items-center gap-1">
                          <Send className="h-3 w-3" /> {b.status === "sent" ? "重发" : "发送"}
                        </button>
                        {b.status !== "sent" && <button onClick={() => handleReject(b.id)} disabled={actionId === b.id} className="px-2 py-1 border border-(--danger) text-(--danger) text-xs font-bold hover:bg-(--danger) hover:text-black">拒绝</button>}
                      </>}
                      <button onClick={() => handleDelete(b)} disabled={actionId === b.id} className="px-2 py-1 border border-(--danger)/60 text-(--danger) text-xs font-bold hover:bg-(--danger) hover:text-black">删除</button>
                      <button className="p-1 text-(--text-muted) hover:text-white">{expandedId === b.id ? <ChevronUp className="h-4 w-4" /> : <ChevronDown className="h-4 w-4" />}</button>
                    </div>
                  </td>
                </tr>
                {expandedId === b.id && detail && (
                  <tr key={`detail-${b.id}`}><td colSpan={6} className="bg-(--bg-surface) p-4">
                    {loadingDetail ? <div className="flex items-center gap-2 text-(--text-muted)"><Loader2 className="h-4 w-4 animate-spin" />加载中...</div> : (
                      <div className="space-y-4">
                        {detail.batch.analysis && <div><h3 className="text-xs font-bold uppercase text-(--text-muted) mb-1">分析</h3><p className="text-sm text-(--text-primary) leading-relaxed">{detail.batch.analysis}</p></div>}
                        {detail.recipients.length > 0 && <div><h3 className="text-xs font-bold uppercase text-(--text-muted) mb-2">收件人列表 ({detail.recipients.length})</h3>
                          <table className="w-full text-xs"><thead className="text-(--text-muted)"><tr><th className="py-1 text-left">邮箱</th><th className="py-1 text-left">姓名</th><th className="py-1 text-left">仓库</th><th className="py-1 text-left">推荐理由</th><th className="py-1 text-left">状态</th></tr></thead>
                            <tbody className="divide-y divide-(--border-divider)">{detail.recipients.map((r) => <tr key={r.id}><td className="py-2 text-(--text-secondary) font-mono">{r.email}</td><td className="py-2 text-(--text-primary)">{r.name}</td><td className="py-2 text-(--text-muted)">{r.repo}</td><td className="py-2 text-(--text-secondary)">{r.reason}</td><td className="py-2">{r.status === "sent" ? <span className="text-(--success)">已发送</span> : r.status === "queued" ? <span className="text-(--info)">队列中</span> : r.status === "failed" ? <span className="text-(--danger)">失败</span> : r.status === "skipped" ? <span className="text-(--text-muted)">跳过</span> : <span className="text-(--text-muted)">待发</span>}</td></tr>)}</tbody></table>
                        </div>}
                      </div>
                    )}
                  </td></tr>
                )}
              </React.Fragment>
            ))}
          </tbody>
        </table>
      </div>

      {/* Send Confirmation Modal */}
      {sendModal && (
        <div className="fixed inset-0 z-50 flex items-start justify-center bg-black/60 p-4 overflow-y-auto" onClick={() => setSendModal(null)}>
          <div className="bg-(--bg-app) border border-(--border-subtle) w-full max-w-5xl my-8 max-h-[90vh] flex flex-col" onClick={e => e.stopPropagation()}>
            <div className="flex items-center justify-between p-4 border-b border-(--border-subtle)">
              <div>
                <h2 className="font-bold text-lg">发送确认 — {sendModal.org_name}</h2>
                <p className="text-xs text-(--text-muted)">{sendStats.filter(s => s.selected).length} / {sendStats.length} 收件人</p>
              </div>
              <button onClick={() => setSendModal(null)} className="text-(--text-muted) hover:text-white"><X className="h-5 w-5" /></button>
            </div>

            {loadingSend ? (
              <div className="flex items-center justify-center p-8"><Loader2 className="h-6 w-6 animate-spin text-(--text-muted)" /></div>
            ) : (
              <div className="flex-1 overflow-y-auto p-4 space-y-4">
                {/* Recipient selection — 已发送 / 未发送 两个列表，均可勾选（含重发） */}
                <div>
                  <h3 className="text-xs font-bold uppercase text-(--text-muted) mb-2">收件人</h3>

                  {/* 未发送 */}
                  <div className="mb-3">
                    <div className="flex items-center justify-between mb-1">
                      <span className="text-[11px] font-bold text-(--text-primary)">未发送（{sendStats.filter(s => s.status !== "sent").length}）</span>
                      <button onClick={() => toggleGroup(false)} className="text-xs border border-(--border-subtle) px-2 py-0.5 text-(--text-secondary) hover:bg-(--bg-elevated)">
                        {sendStats.filter(s => s.status !== "sent").every(s => s.selected) ? "取消全选" : "全选"}
                      </button>
                    </div>
                    <div className="border border-(--border-subtle) max-h-40 overflow-y-auto">
                      <table className="w-full text-xs">
                        <thead className="bg-(--bg-elevated) text-(--text-muted) sticky top-0"><tr><th className="py-1 px-2 text-left w-8"></th><th className="py-1 px-2 text-left">邮箱</th><th className="py-1 px-2 text-left">姓名</th><th className="py-1 px-2 text-right w-14">历史</th><th className="py-1 px-2 text-right w-16">状态</th></tr></thead>
                        <tbody className="divide-y divide-(--border-divider)">
                          {sendStats.filter(s => s.status !== "sent").map(s => (
                            <tr key={s.id} onClick={() => toggleRecipient(s.id)} className={`cursor-pointer transition-colors ${s.selected ? "bg-(--bg-elevated)" : "opacity-50 hover:opacity-75"}`}>
                              <td className="py-1 px-2"><input type="checkbox" checked={s.selected} onChange={() => {}} className="accent-white" /></td>
                              <td className="py-1 px-2 text-(--text-secondary) font-mono text-[11px]">{s.email}</td>
                              <td className="py-1 px-2 text-(--text-primary)">{s.name}</td>
                              <td className="py-1 px-2 text-right text-(--text-muted)">{s.count > 0 ? <span className="text-(--warning)">{s.count}</span> : "0"}</td>
                              <td className="py-1 px-2 text-right text-xs">{s.status === "queued" ? <span className="text-(--info)">队列中</span> : s.status === "skipped" ? <span className="text-(--text-muted)">跳过</span> : s.status === "failed" ? <span className="text-(--danger)">失败</span> : "待发"}</td>
                            </tr>
                          ))}
                          {sendStats.filter(s => s.status !== "sent").length === 0 && <tr><td colSpan={5} className="py-4 text-center text-(--text-muted)">没有未发送的收件人</td></tr>}
                        </tbody>
                      </table>
                    </div>
                  </div>

                  {/* 已发送（可勾选重新发送） */}
                  <div>
                    <div className="flex items-center justify-between mb-1">
                      <span className="text-[11px] font-bold text-(--text-primary)">已发送（{sendStats.filter(s => s.status === "sent").length}）— 可勾选再次发送</span>
                      <button onClick={() => toggleGroup(true)} className="text-xs border border-(--border-subtle) px-2 py-0.5 text-(--text-secondary) hover:bg-(--bg-elevated)">
                        {sendStats.filter(s => s.status === "sent").length > 0 && sendStats.filter(s => s.status === "sent").every(s => s.selected) ? "取消全选" : "全选"}
                      </button>
                    </div>
                    <div className="border border-(--border-subtle) max-h-40 overflow-y-auto">
                      <table className="w-full text-xs">
                        <thead className="bg-(--bg-elevated) text-(--text-muted) sticky top-0"><tr><th className="py-1 px-2 text-left w-8"></th><th className="py-1 px-2 text-left">邮箱</th><th className="py-1 px-2 text-left">姓名</th><th className="py-1 px-2 text-right w-14">历史</th><th className="py-1 px-2 text-right w-16">状态</th></tr></thead>
                        <tbody className="divide-y divide-(--border-divider)">
                          {sendStats.filter(s => s.status === "sent").map(s => (
                            <tr key={s.id} onClick={() => toggleRecipient(s.id)} className={`cursor-pointer transition-colors ${s.selected ? "bg-(--bg-elevated)" : "opacity-50 hover:opacity-75"}`}>
                              <td className="py-1 px-2"><input type="checkbox" checked={s.selected} onChange={() => {}} className="accent-white" /></td>
                              <td className="py-1 px-2 text-(--text-secondary) font-mono text-[11px]">{s.email}</td>
                              <td className="py-1 px-2 text-(--text-primary)">{s.name}</td>
                              <td className="py-1 px-2 text-right text-(--text-muted)">{s.count > 0 ? <span className="text-(--warning)">{s.count}</span> : "0"}</td>
                              <td className="py-1 px-2 text-right text-(--success)">已发</td>
                            </tr>
                          ))}
                          {sendStats.filter(s => s.status === "sent").length === 0 && <tr><td colSpan={5} className="py-4 text-center text-(--text-muted)">没有已发送的收件人</td></tr>}
                        </tbody>
                      </table>
                    </div>
                  </div>
                </div>

                {/* Email Sender selection */}                <div>                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">发信服务商 / 邮箱账号</label>                  <select                    value={sendSenderId ?? ""}                    onChange={e => setSendSenderId(e.target.value ? Number(e.target.value) : undefined)}                    className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white font-mono"                  >                    <option value="">🎲 随机邮箱 (从数据库多邮箱配置中随机选择发信)</option>                    {senders.map(s => (                      <option key={s.id} value={s.id} disabled={!s.enabled}>                        {s.name} — {s.from_address} ({s.host}:{s.port}){s.is_default ? " [默认]" : ""}{!s.enabled ? " (已禁用)" : ""}                      </option>                    ))}                  </select>                </div>                {/* CC */}                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">CC (逗号分隔)</label>
                  <input value={sendCc} onChange={e => setSendCc(e.target.value)} placeholder="cc@example.com, cc2@example.com" className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white" />
                </div>

                {/* Subject */}
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">邮件主题</label>
                  <input value={sendSubject} onChange={e => setSendSubject(e.target.value)} className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white font-bold" />
                </div>

                {/* Content editor */}
                <div>
                  <label className="block text-[10px] uppercase text-(--text-muted) mb-1 font-bold">邮件内容 (HTML)</label>
                  <RichEditor value={sendContent} onChange={setSendContent} minHeight={400} />
                </div>
              </div>
            )}

            {/* Footer actions */}
            <div className="flex items-center justify-between p-4 border-t border-(--border-subtle)">
              <button onClick={() => setSendModal(null)} className="border border-(--border-subtle) px-4 py-2 text-sm text-(--text-secondary) hover:bg-(--bg-elevated)">取消</button>
              <button onClick={handleSendConfirm} disabled={actionId === sendModal.id || loadingSend} className="bg-(--success) text-black font-bold px-6 py-2 text-sm hover:bg-(--success)/80 disabled:opacity-50 flex items-center gap-2">
                {actionId === sendModal.id ? <Loader2 className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
                发送给 {sendStats.filter(s => s.selected).length} 人
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
