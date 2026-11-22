import { useState } from "react";
import { Loader2, RefreshCw, ExternalLink } from "lucide-react";
import {
    fetchRawItems,
    fetchStatus,
    type RawItem,
    type SystemStatus,
} from "../api";
import { StatsHeader, Pagination } from "@kit";
import { fmtTime } from "../utils";
import { useAsync } from "../hooks";

type SourceFilter = "all" | "x" | "github" | "reddit";

export function RawItems() {
    const [sourceFilter, setSourceFilter] = useState<SourceFilter>("all");
    const [page, setPage] = useState(1);

    // 部分成功也要展示：两路 allSettled，子请求失败原因并入返回值
    const { data, loading, error, reload: load } = useAsync<{
        rawItems: RawItem[];
        total: number;
        statusData: SystemStatus | null;
        partialError: string | null;
    }>(async () => {
        const src = sourceFilter === "all" ? undefined : sourceFilter;
        const [itemsRes, sysRes] = await Promise.allSettled([
            fetchRawItems(src, page),
            fetchStatus(),
        ]);
        const errors: string[] = [];
        for (const r of [itemsRes, sysRes]) {
            if (r.status === "rejected") errors.push(r.reason instanceof Error ? r.reason.message : String(r.reason));
        }
        return {
            rawItems: itemsRes.status === "fulfilled" ? itemsRes.value.items : [],
            total: itemsRes.status === "fulfilled" ? itemsRes.value.total : 0,
            statusData: sysRes.status === "fulfilled" ? sysRes.value : null,
            partialError: errors.length > 0 ? errors.join("；") : null,
        };
    }, [sourceFilter, page]);
    const rawItems = data?.rawItems ?? [];
    const total = data?.total ?? 0;
    const statusData = data?.statusData ?? null;
    // 部分成功（子请求失败）的提示：派生自最近一次取数结果，无需额外 effect
    const partialErr = data?.partialError ?? null;
    return (
        <div className="flex-1 p-4 overflow-y-auto">
            <header className="mb-8 flex items-center justify-between">
                <div>
                    <h1 className="text-2xl font-bold tracking-tighter uppercase">
                        Raw Material Library
                    </h1>
                    <p className="mt-1 text-sm text-(--text-muted)">
                        浏览已抓取的原始素材数据
                    </p>
                </div>
                <button
                    onClick={load}
                    disabled={loading}
                    className="border border-(--border-subtle) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-elevated) transition flex items-center gap-2"
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
            <div className="mb-4 flex flex-wrap gap-2">
                {(
                    [
                        "all",
                        "x",
                        "github",
                        "reddit",
                    ] as SourceFilter[]
                ).map((src) => (
                    <button
                        key={src}
                        onClick={() => setSourceFilter(src)}
                        className={`border px-3 py-1 text-xs font-bold uppercase ${sourceFilter === src ? "border-white bg-(--accent) text-(--accent-foreground)" : "border-(--border-subtle) text-(--text-secondary) hover:bg-(--bg-elevated)"}`}
                    >
                        {src === "all" ? "全部来源" : src}
                    </button>
                ))}
            </div>
            <div className="border border-(--border-subtle) ">
                <table className="w-full text-left text-sm">
                    <thead className="border-b border-(--border-subtle) text-(--text-muted)">
                        <tr>
                            <th className="px-4 py-3 font-bold uppercase">
                                ID
                            </th>
                            <th className="px-4 py-3 font-bold uppercase">
                                来源
                            </th>
                            <th className="px-4 py-3 font-bold uppercase">
                                关键词
                            </th>
                            <th className="px-4 py-3 font-bold uppercase">
                                素材内容概要
                            </th>
                            <th className="px-4 py-3 font-bold uppercase">
                                同步日期
                            </th>
                            <th className="px-4 py-3 text-right font-bold uppercase">
                                链接
                            </th>
                        </tr>
                    </thead>
                    <tbody className="divide-y divide-(--border-divider)">
                        {rawItems.length === 0 ? (
                            <tr>
                                <td
                                    colSpan={6}
                                    className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs"
                                >
                                    NO MATERIALS FOUND
                                </td>
                            </tr>
                        ) : (
                            rawItems.map((item) => (
                                <tr
                                    key={item.id}
                                    className="hover:bg-(--bg-elevated) transition-colors"
                                >
                                    <td className="px-4 py-3 font-mono text-(--text-secondary)">
                                        {item.id}
                                    </td>
                                    <td className="px-4 py-3 uppercase font-bold text-xs">
                                        <span className="px-1.5 py-0.5 border border-(--border-subtle) bg-(--bg-elevated)">
                                            {item.source}
                                        </span>
                                    </td>
                                    <td className="px-4 py-3 text-xs text-(--text-secondary)">
                                        {item.keyword || "-"}
                                    </td>
                                    <td className="px-4 py-3 max-w-xl">
                                        <p
                                            className="truncate text-(--text-primary)"
                                            title={item.content}
                                        >
                                            {item.content}
                                        </p>
                                    </td>
                                    <td className="px-4 py-3 text-xs text-(--text-muted)">
                                        {fmtTime(item.fetched_at)}
                                    </td>
                                    <td className="px-4 py-3 text-right">
                                        {item.url ? (
                                            <a
                                                href={item.url}
                                                target="_blank"
                                                rel="noreferrer"
                                                className="inline-flex items-center text-(--text-secondary) hover:text-white transition-colors"
                                            >
                                                <ExternalLink className="h-3.5 w-3.5" />
                                            </a>
                                        ) : (
                                            <span className="text-(--text-disabled)">
                                                -
                                            </span>
                                        )}
                                    </td>
                                </tr>
                            ))
                        )}
                    </tbody>
                </table>
                <Pagination
                    page={page}
                    pageSize={20}
                    total={total}
                    onPageChange={setPage}
                />
            </div>
        </div>
    );
}
