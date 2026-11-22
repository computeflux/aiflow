import { useState } from "react";
import { Link } from "react-router-dom";
import {
    Loader2,
    RefreshCw,
    AlertTriangle,
} from "lucide-react";
import {
    fetchJobs,
    fetchStatus,
    approveJob,
    approveEmailBatch,
    rejectEmailBatch,
    fetchEmailBatches,
    fetchErrorLogs,
    type PublishJob,
    type EmailBatch,
    type SystemStatus,
    type ErrorLog,
} from "../api";
import { StatsHeader } from "@kit";
import { useAsync } from "../hooks";
import { fmtTime } from "../utils";

const jobStatusLabels: Record<string, string> = {
    pending: "待审批",
    approved: "已通过",
    published: "已发布",
    rejected: "已拒绝",
    failed: "失败",
};
const jobStatusClasses: Record<string, string> = {
    pending: "border-(--border-subtle) bg-(--bg-elevated) text-(--warning)",
    approved: "border-(--border-subtle) bg-(--bg-elevated) text-(--success)",
    published: "border-(--border-subtle) bg-(--bg-elevated) text-(--info)",
    rejected: "border-(--border-subtle) bg-(--bg-elevated) text-(--danger)",
    failed: "border-(--border-subtle) bg-(--bg-elevated) text-[#F43F5E]",
};
const emailStatusLabels: Record<string, string> = {
    analyzing: "分析中",
    pending: "待审批",
    sending: "发送中",
    sent: "已发送",
    failed: "失败",
    rejected: "已拒绝",
};
const emailStatusColors: Record<string, string> = {
    analyzing: "text-(--purple)",
    pending: "text-(--warning)",
    sending: "text-(--purple)",
    sent: "text-(--success)",
    failed: "text-(--danger)",
    rejected: "text-(--text-muted)",
};

type ApproveTab = "jobs" | "emails";
type JobFilter = "pending" | "approved" | "published" | "rejected" | "all";

export function Console() {
    const [approveTab, setApproveTab] = useState<ApproveTab>("jobs");
    const [jobFilter, setJobFilter] = useState<JobFilter>("pending");
    const [actionId, setActionId] = useState<number | null>(null);

    // 部分成功也要展示：四路 allSettled，子请求失败原因并入返回值（不阻断其余面板）
    interface ConsoleData {
        jobs: PublishJob[];
        emails: EmailBatch[];
        statusData: SystemStatus | null;
        recentErrors: ErrorLog[];
        partialError: string | null;
    }
    const reason = (r: PromiseRejectedResult): string =>
        r.reason instanceof Error ? r.reason.message : String(r.reason);
    const { data, loading, error, reload: load, setError } = useAsync<ConsoleData>(async () => {
        const status = jobFilter === "all" ? undefined : jobFilter;
        const [jobsRes, sysRes, emailRes, errRes] = await Promise.allSettled([
            fetchJobs(status),
            fetchStatus(),
            fetchEmailBatches(),
            fetchErrorLogs({ resolved: "false", page: 1, page_size: 5 }),
        ]);
        const errors: string[] = [];
        for (const r of [jobsRes, sysRes, emailRes, errRes]) {
            if (r.status === "rejected") errors.push(reason(r));
        }
        return {
            jobs: jobsRes.status === "fulfilled" ? jobsRes.value : [],
            emails: emailRes.status === "fulfilled" ? emailRes.value.batches : [],
            statusData: sysRes.status === "fulfilled" ? sysRes.value : null,
            recentErrors: errRes.status === "fulfilled" ? errRes.value.items : [],
            partialError: errors.length > 0 ? errors.join("；") : null,
        };
    }, [jobFilter]);
    const jobs = data?.jobs ?? [];
    const emails = data?.emails ?? [];
    const statusData = data?.statusData ?? null;
    const recentErrors = data?.recentErrors ?? [];
    // 部分成功（子请求失败）的提示：派生自最近一次取数结果，无需额外 effect
    const partialErr = data?.partialError ?? null;

    const handleJobApprove = async (id: number) => {
        setActionId(id);
        try {
            await approveJob(id);
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setActionId(null);
        }
    };
    const handleEmailApprove = async (id: number) => {
        setActionId(id);
        try {
            await approveEmailBatch(id);
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setActionId(null);
        }
    };
    const handleEmailReject = async (id: number) => {
        setActionId(id);
        try {
            await rejectEmailBatch(id);
            await load();
        } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
        } finally {
            setActionId(null);
        }
    };

    return (
        <div className="flex-1 p-4 overflow-y-auto">
            <header className="mb-8 flex items-center justify-between">
                <div>
                    <h1 className="text-2xl font-bold tracking-tighter uppercase">
                        Workflow Console
                    </h1>
                    <p className="mt-1 text-sm text-(--text-muted)">
                        管理审批队列和邮件批次
                    </p>
                </div>
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
            </header>
            {(error || partialErr) && (
                <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">
                    {error ?? partialErr}
                </div>
            )}
            <StatsHeader statusData={statusData} />

            {/* Approval Tabs */}
            <div className="mb-4 flex items-center gap-2 px-2 border border-(--border-subtle)">
                <button
                    onClick={() => setApproveTab("jobs")}
                    className={`px-4 py-2 text-xs font-bold uppercase transition ${approveTab === "jobs" ? "border-b-2 border-white text-white" : "text-(--text-muted) hover:text-(--text-secondary)"}`}
                >
                    发布审批
                </button>
                <button
                    onClick={() => setApproveTab("emails")}
                    className={`px-4 py-2 text-xs font-bold uppercase transition ${approveTab === "emails" ? "border-b-2 border-white text-white" : "text-(--text-muted) hover:text-(--text-secondary)"}`}
                >
                    邮件审批 (
                    {
                        emails.filter(
                            (e) =>
                                e.status === "pending" ||
                                e.status === "analyzing",
                        ).length
                    }
                    )
                </button>
            </div>

            {/* Jobs Tab */}
            {approveTab === "jobs" && (
                <>
                    <div className="mb-4 flex flex-wrap gap-2">
                        {(
                            [
                                "pending",
                                "approved",
                                "published",
                                "rejected",
                                "all",
                            ] as JobFilter[]
                        ).map((s) => (
                            <button
                                key={s}
                                onClick={() => setJobFilter(s)}
                                className={`border px-3 py-1 text-xs font-bold uppercase ${jobFilter === s ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
                            >
                                {jobStatusLabels[s] || s}
                            </button>
                        ))}
                    </div>
                    <div className="border border-(--border-subtle) mb-8">
                        <table className="w-full text-left text-sm">
                            <thead className="border-b border-(--border-subtle) text-(--text-muted)">
                                <tr>
                                    <th className="px-4 py-3 font-bold uppercase">
                                        ID
                                    </th>
                                    <th className="px-4 py-3 font-bold uppercase">
                                        平台
                                    </th>
                                    <th className="px-4 py-3 font-bold uppercase">
                                        状态
                                    </th>
                                    <th className="px-4 py-3 text-right font-bold uppercase">
                                        操作
                                    </th>
                                </tr>
                            </thead>
                            <tbody className="divide-y divide-(--border-divider)">
                                {jobs.length === 0 ? (
                                    <tr>
                                        <td
                                            colSpan={4}
                                            className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs"
                                        >
                                            NO JOBS
                                        </td>
                                    </tr>
                                ) : (
                                    jobs.map((j) => (
                                        <tr
                                            key={j.id}
                                            className="hover:bg-(--bg-elevated)"
                                        >
                                            <td className="px-4 py-3 font-mono text-(--text-secondary)">
                                                {j.id}
                                            </td>
                                            <td className="px-4 py-3 uppercase font-bold">
                                                {j.platform}
                                            </td>
                                            <td className="px-4 py-3 uppercase text-xs font-bold">
                                                <span
                                                    className={`px-2 py-0.5 border ${jobStatusClasses[j.status] || ""}`}
                                                >
                                                    {jobStatusLabels[
                                                        j.status
                                                    ] || j.status}
                                                </span>
                                            </td>
                                            <td className="px-4 py-3 text-right">
                                                {j.status === "pending" && (
                                                    <button
                                                        onClick={() =>
                                                            handleJobApprove(
                                                                j.id,
                                                            )
                                                        }
                                                        disabled={
                                                            actionId === j.id
                                                        }
                                                        className="text-white underline font-bold disabled:opacity-50"
                                                    >
                                                        {actionId === j.id
                                                            ? "PROC..."
                                                            : "通过"}
                                                    </button>
                                                )}
                                            </td>
                                        </tr>
                                    ))
                                )}
                            </tbody>
                        </table>
                    </div>
                </>
            )}

            {/* Emails Tab */}
            {approveTab === "emails" && (
                <div className="border border-(--border-subtle) mb-8">
                    <table className="w-full text-left text-sm">
                        <thead className="border-b border-(--border-subtle) text-(--text-muted)">
                            <tr>
                                <th className="px-4 py-3 font-bold uppercase">
                                    ID
                                </th>
                                <th className="px-4 py-3 font-bold uppercase">
                                    组织
                                </th>
                                <th className="px-4 py-3 font-bold uppercase">
                                    状态
                                </th>
                                <th className="px-4 py-3 font-bold uppercase">
                                    收件人
                                </th>
                                <th className="px-4 py-3 font-bold uppercase">
                                    时间
                                </th>
                                <th className="px-4 py-3 text-right font-bold uppercase">
                                    操作
                                </th>
                            </tr>
                        </thead>
                        <tbody className="divide-y divide-(--border-divider)">
                            {emails.length === 0 ? (
                                <tr>
                                    <td
                                        colSpan={6}
                                        className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs"
                                    >
                                        NO EMAIL BATCHES
                                    </td>
                                </tr>
                            ) : (
                                emails.map((e) => (
                                    <tr
                                        key={e.id}
                                        className="hover:bg-(--bg-elevated)"
                                    >
                                        <td className="px-4 py-3 font-mono text-(--text-secondary)">
                                            {e.id}
                                        </td>
                                        <td className="px-4 py-3 text-(--text-primary) font-bold">
                                            {e.org_name}
                                        </td>
                                        <td className="px-4 py-3">
                                            <span
                                                className={`text-xs font-bold uppercase ${emailStatusColors[e.status] || ""}`}
                                            >
                                                {emailStatusLabels[e.status] ||
                                                    e.status}
                                            </span>
                                        </td>
                                        <td className="px-4 py-3 text-(--text-secondary)">
                                            {e.sent}/{e.total}
                                        </td>
                                        <td className="px-4 py-3 text-(--text-muted) text-xs">
                                            {fmtTime(e.created_at)}
                                        </td>
                                        <td className="px-4 py-3 text-right">
                                            <div className="flex items-center justify-end gap-1">
                                                {e.status === "pending" && (
                                                    <>
                                                        <button
                                                            onClick={() =>
                                                                handleEmailApprove(
                                                                    e.id,
                                                                )
                                                            }
                                                            disabled={
                                                                actionId ===
                                                                e.id
                                                            }
                                                            className="px-2 py-1 border border-(--success) text-(--success) text-xs font-bold hover:bg-(--success) hover:text-black"
                                                        >
                                                            批准并发送
                                                        </button>
                                                        <button
                                                            onClick={() =>
                                                                handleEmailReject(
                                                                    e.id,
                                                                )
                                                            }
                                                            disabled={
                                                                actionId ===
                                                                e.id
                                                            }
                                                            className="px-2 py-1 border border-(--danger) text-(--danger) text-xs font-bold hover:bg-(--danger) hover:text-black"
                                                        >
                                                            拒绝
                                                        </button>
                                                    </>
                                                )}
                                            </div>
                                        </td>
                                    </tr>
                                ))
                            )}
                        </tbody>
                    </table>
                </div>
            )}

            {/* 最近错误 */}
            <div className="border border-(--border-subtle) mb-8">
                <div className="flex items-center justify-between px-4 py-3 border-b border-(--border-subtle)">
                    <span className="text-xs font-bold uppercase text-(--text-muted) flex items-center gap-2">
                        <AlertTriangle className="h-4 w-4 text-(--danger)" />
                        最近错误（未处理）
                    </span>
                    <Link to="/ops/errors" className="text-xs font-bold uppercase text-(--text-secondary) hover:text-white">
                        查看全部 →
                    </Link>
                </div>
                {recentErrors.length === 0 ? (
                    <div className="px-4 py-6 text-center text-(--text-muted) font-mono text-xs">NO ERRORS</div>
                ) : (
                    <table className="w-full text-left text-sm">
                        <thead className="border-b border-(--border-subtle) text-(--text-muted)">
                            <tr>
                                <th className="px-4 py-2 font-bold uppercase">时间</th>
                                <th className="px-4 py-2 font-bold uppercase">系统</th>
                                <th className="px-4 py-2 font-bold uppercase">来源</th>
                                <th className="px-4 py-2 font-bold uppercase">消息</th>
                            </tr>
                        </thead>
                        <tbody className="divide-y divide-(--border-divider)">
                            {recentErrors.map((e) => (
                                <tr key={e.id} className="hover:bg-(--bg-elevated)">
                                    <td className="px-4 py-2 text-(--text-muted) font-mono text-xs">
                                        {fmtTime(e.created_at)}
                                    </td>
                                    <td className="px-4 py-2 uppercase font-bold text-xs">{e.system}</td>
                                    <td className="px-4 py-2 font-mono text-xs text-(--text-secondary) truncate max-w-40" title={e.source}>{e.source}</td>
                                    <td className="px-4 py-2 text-(--text-secondary) truncate max-w-120 text-red-400" title={e.message}>{e.message}</td>
                                </tr>
                            ))}
                        </tbody>
                    </table>
                )}
            </div>
</div>
    );
}
