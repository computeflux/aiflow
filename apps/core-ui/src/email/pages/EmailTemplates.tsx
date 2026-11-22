import { useState } from "react";
import { Loader2, Plus, Edit3, Trash2, Check, FileText } from "lucide-react";
import { fetchEmailTemplates, createEmailTemplate, updateEmailTemplate, deleteEmailTemplate, type EmailTemplate } from "../api";
import { useAsync } from "../hooks"
import { ConfirmModal, RichEditor } from "@kit";

const DEFAULT_TEMPLATES: Array<{ name: string; subject: string; body: string; content_type: string }> = [
  {
    name: "ComputeFlux 技术交流（英文 HTML）",
    subject: "A technical note for {{.OrgName}} contributors",
    body: `<html><body style="background:transparent;padding:32px 16px"><div style="max-width:600px;margin:0 auto;padding:6px;border:1px solid #000"><div style="background:#000;color:#fff;font-family:monospace;padding:40px 28px;font-size:13px;line-height:1.7"><p style="margin-bottom:24px">Hi {{.Name}},</p><p style="margin-bottom:20px">I came across your work in the <strong>Polkadot ecosystem</strong> and thought I'd say hi.</p><p style="margin-bottom:20px">I'm part of a small team exploring <strong>ComputeFlux</strong> — a decentralized AI gateway built on TEE. The idea is simple: make AI compute fully anonymous and permissionless.</p><p style="margin-bottom:24px;font-size:12px;color:#888;text-transform:uppercase;letter-spacing:1px">A few things we've been thinking about:</p><p style="margin-bottom:10px;padding-left:14px;border-left:2px solid #333"><span style="color:#fff">No KYC, no tracking.</span> <span style="color:#888">TEE guarantees even the node operator can't peek at your prompts or data.</span></p><p style="margin-bottom:10px;padding-left:14px;border-left:2px solid #333"><span style="color:#fff">Permissionless compute market.</span> <span style="color:#888">Anyone contributes idle GPUs or unused AI quotas. Anyone accesses them anonymously with DOT, USDT or USDC.</span></p><p style="margin-bottom:16px;padding-left:14px;border-left:2px solid #333"><span style="color:#fff">Trust the code, not the operator.</span> <span style="color:#888">Every inference verifiable on-chain via TEE attestation.</span></p><p style="margin-bottom:20px">We think other Polkadot builders might find this interesting. Would love to hear what you think.</p><p style="margin-bottom:28px"><a href="https://computeflux.com" style="display:inline-block;background:#fff;color:#000;padding:10px 24px;text-decoration:none;font-weight:bold;font-size:12px">Join the conversation & claim your free compute</a></p><p style="margin-bottom:8px">Cheers,</p><p style="color:#555;font-size:12px">— ComputeFlux</p></div></div></body></html>`,
    content_type: "html",
  },
  {
    name: "ComputeFlux 技术交流（中文纯文本）",
    subject: "关于 {{.OrgName}} 项目的一点技术交流",
    body: `{{.OrgName}} 生态的伙伴你好，抱歉发邮件打扰您。

最近关注到你在 Polkadot 生态的一些工作，我也在这个领域折腾，想和你随便聊聊。

我们在做 ComputeFlux，一个基于 TEE 的去中心化 AI 网关。本质上就是让 AI 推理完全匿名、无需许可 —— 贡献者共享闲置 GPU 和 AI 配额，使用者通过 DOT / USDT / USDC 匿名调用，TEE 确保连节点运营方也看不到你的数据。每一笔推理都在链上可验证，信任代码而非人。

我们觉得 Polkadot 生态关注隐私的开发者们可能对这个方向感兴趣，想听听你的看法。

加入交流 & 获取你的专属免费算力：https://computeflux.com

祝好！

ComputeFlux`,
    content_type: "text",
  },
  {
    name: "ComputeFlux 通用交流（纯文本）",
    subject: "A brief technical note",
    body: `Hi {{.Name}},

Saw your work in the Polkadot ecosystem — nice stuff. Thought I'd share something we've been hacking on in case it's relevant.

ComputeFlux is a TEE-based decentralized AI gateway. Anonymous inference, permissionless compute (people share idle GPUs, others pay with DOT/USDT/USDC), every call verifiable on-chain. No KYC, no tracking, trust the TEE not the operator.

Would love to hear your take on this.

Join the conversation & claim your free compute: https://computeflux.com

— ComputeFlux`,
    content_type: "text",
  },
];

export function EmailTemplates() {
  const [deleteConfirm, setDeleteConfirm] = useState<number | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [editingTpl, setEditingTpl] = useState<EmailTemplate | null>(null);
  const [tplName, setTplName] = useState("");
  const [tplSubject, setTplSubject] = useState("");
  const [tplBody, setTplBody] = useState("");
  const [tplContentType, setTplContentType] = useState("text");
  const [tplDefault, setTplDefault] = useState(false);
  const [actionId, setActionId] = useState<number | null>(null);
  const [pickDefault, setPickDefault] = useState("");

  const { data: templates, loading, error, reload: load, setError } = useAsync<EmailTemplate[]>(async () => (await fetchEmailTemplates()).templates, []);

  const resetForm = () => { setShowForm(false); setEditingTpl(null); setPickDefault(""); setTplName(""); setTplSubject(""); setTplBody(""); setTplContentType("text"); setTplDefault(false); };

  const applyDefault = (idx: number) => {
    const t = DEFAULT_TEMPLATES[idx];
    setPickDefault(t.name);
    setTplName(t.name);
    setTplSubject(t.subject);
    setTplBody(t.body);
    setTplContentType(t.content_type);
  };

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!tplName.trim() || !tplSubject.trim()) return;
    const data = { name: tplName.trim(), subject: tplSubject.trim(), body: tplBody, content_type: tplContentType, is_default: tplDefault };
    try {
      if (editingTpl && editingTpl.id) await updateEmailTemplate(editingTpl.id, data);
      else await createEmailTemplate(data);
      resetForm(); await load();
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
  };

  const handleEdit = (tpl: EmailTemplate) => { setEditingTpl(tpl); setPickDefault(""); setTplName(tpl.name); setTplSubject(tpl.subject); setTplBody(tpl.body); setTplContentType(tpl.content_type || "text"); setTplDefault(tpl.is_default); setShowForm(true); };
  const handleDelete = async () => { if (!deleteConfirm) return; const id = deleteConfirm; setDeleteConfirm(null); setActionId(id); try { await deleteEmailTemplate(id); await load(); } catch (err) { setError(err instanceof Error ? err.message : String(err)); } finally { setActionId(null); } };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase flex items-center gap-2"><FileText className="h-6 w-6" /> 邮件模板</h1>
          <p className="mt-1 text-sm text-(--text-muted)">管理邮件发送模板，设置默认模板用于批量发送</p>
        </div>
        <button onClick={() => { resetForm(); setShowForm(!showForm); }} className="border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) transition flex items-center gap-2">
          <Plus className="h-4 w-4" />{showForm ? "取消" : "新建模板"}
        </button>
      </header>

      {error && <div className="mb-4 border border-(--danger)/50 bg-(--danger)/10 p-3 text-sm text-(--danger)">{error}</div>}

      {showForm && (
        <form onSubmit={handleSave} className="mb-8 border border-(--border-subtle)  p-4 space-y-3">
          <h3 className="text-xs font-bold text-(--text-secondary)">{editingTpl ? `编辑模板 #${editingTpl.id}` : "新建模板"}</h3>

          {!editingTpl && (
            <div>
              <label className="block text-[10px] uppercase text-(--text-muted) mb-1">选择基础模板</label>
              <select value={pickDefault} onChange={e => { const idx = parseInt(e.target.value); if (idx >= 0) applyDefault(idx); }} className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white w-full">
                <option value="">空白模板</option>
                {DEFAULT_TEMPLATES.map((t, i) => <option key={i} value={i}>{t.name}</option>)}
              </select>
            </div>
          )}

          <div className="flex flex-wrap gap-3">
            <div className="flex-1 min-w-[200px]"><label className="block text-[10px] uppercase text-(--text-muted) mb-1">模板名称</label><input value={tplName} onChange={e => setTplName(e.target.value)} placeholder="模板名称" className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white" /></div>
            <label className="flex items-center gap-2 pt-5"><input type="checkbox" checked={tplDefault} onChange={e => setTplDefault(e.target.checked)} className="accent-white" /><span className="text-xs text-(--text-muted)">默认模板</span></label>
          </div>
          <div><label className="block text-[10px] uppercase text-(--text-muted) mb-1">邮件主题</label><input value={tplSubject} onChange={e => setTplSubject(e.target.value)} placeholder="邮件主题" className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white" /></div>
          <div><label className="block text-[10px] uppercase text-(--text-muted) mb-1">内容类型</label><select value={tplContentType} onChange={e => setTplContentType(e.target.value)} className="bg-(--bg-elevated) border border-(--border-subtle) px-3 py-1.5 text-sm focus:outline-none focus:border-white"><option value="text">纯文本 (text/plain)</option><option value="html">HTML (text/html)</option></select></div>
          <div><label className="block text-[10px] uppercase text-(--text-muted) mb-1">邮件正文</label>
            {tplContentType === "html" ? (
              <RichEditor value={tplBody} onChange={setTplBody} minHeight={800} />
            ) : (
              <textarea value={tplBody} onChange={e => setTplBody(e.target.value)} rows={8} placeholder="邮件正文内容..." className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-3 py-2 text-sm focus:outline-none focus:border-white resize-y font-mono" />
            )}
          </div>
          <div className="flex gap-2">
            <button type="submit" className="bg-(--accent) text-(--accent-foreground) font-bold px-4 py-1.5 text-sm hover:bg-(--text-primary)">{editingTpl ? "保存修改" : "保存到数据库"}</button>
            <button type="button" onClick={resetForm} className="border border-(--border-subtle) text-(--text-secondary) px-4 py-1.5 text-sm hover:bg-(--bg-elevated)">取消</button>
          </div>
        </form>
      )}

      <div className="border border-(--border-subtle) ">
        <table className="w-full text-left text-sm">
          <thead className="border-b border-(--border-subtle) text-(--text-muted)"><tr><th className="px-4 py-3 font-bold uppercase w-12">#</th><th className="px-4 py-3 font-bold uppercase">模板名称</th><th className="px-4 py-3 font-bold uppercase">类型</th><th className="px-4 py-3 font-bold uppercase">主题</th><th className="px-4 py-3 font-bold uppercase w-24">默认</th><th className="px-4 py-3 text-right font-bold uppercase w-24">操作</th></tr></thead>
          <tbody className="divide-y divide-(--border-divider)">
            {loading ? <tr><td colSpan={6} className="px-4 py-8 text-center"><Loader2 className="h-5 w-5 animate-spin mx-auto text-(--text-muted)" /></td></tr>
            : (templates?.length ?? 0) === 0 ? <tr><td colSpan={6} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">暂无模板，点击「新建模板」创建</td></tr>
            : (templates ?? []).map(tpl => (
              <tr key={tpl.id} className="hover:bg-(--bg-elevated) transition-colors">
                <td className="px-4 py-3 font-mono text-(--text-secondary) text-xs">{tpl.id}</td>
                <td className="px-4 py-3 text-(--text-primary) font-bold">{tpl.name}</td>
                <td className="px-4 py-3"><span className="px-1.5 py-0.5 border border-(--border-subtle) text-[10px] font-bold uppercase text-(--text-muted)">{tpl.content_type || "text"}</span></td>
                <td className="px-4 py-3 text-(--text-secondary) text-xs truncate max-w-[400px]">{tpl.subject}</td>
                <td className="px-4 py-3">{tpl.is_default ? <Check className="h-4 w-4 text-(--success)" /> : <span className="text-(--text-disabled)">-</span>}</td>
                <td className="px-4 py-3 text-right">
                  <div className="flex items-center justify-end gap-1">
                    <button onClick={() => handleEdit(tpl)} className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-white hover:bg-(--bg-hover)"><Edit3 className="h-3 w-3" /></button>
                    <button onClick={() => setDeleteConfirm(tpl.id)} disabled={actionId === tpl.id} className="p-1.5 border border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:text-red-500 hover:border-red-500 disabled:opacity-50"><Trash2 className="h-3 w-3" /></button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {deleteConfirm && (
        <ConfirmModal
          open={true}
          title="删除模板"
          message="确认删除此模板？"
          onConfirm={handleDelete}
          onCancel={() => setDeleteConfirm(null)}
        />
      )}
</div>
  );
}
