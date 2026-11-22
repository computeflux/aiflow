import { useState } from "react";
import { Loader2, ExternalLink, RefreshCw } from "lucide-react";

// 链路追踪（Jaeger UI 同源 iframe，经 nginx /jaeger/ 反代）
// 黑白极简：仅保留头部说明 + iframe，其余交给 Jaeger 自身界面
export function Tracing() {
    const [ready, setReady] = useState(false);
    const [key, setKey] = useState(0);

    return (
        <div className="flex-1 flex flex-col min-w-0">
            <header className="flex items-center justify-between px-4 py-3 border-b border-(--border-default) bg-(--bg-sidebar)">
                <div>
                    <h1 className="text-sm font-bold tracking-tighter uppercase text-white">链路追踪</h1>
                    <p className="mt-0.5 text-[11px] text-(--text-muted)">
                        OpenTelemetry + Jaeger · API / Go Worker / 抓取 Worker / 点赞 Worker 全链路
                    </p>
                </div>
                <div className="flex items-center gap-2">
                    <a
                        href="/jaeger/search"
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex items-center gap-1.5 border border-(--border-default) bg-(--bg-elevated) px-3 py-1.5 text-[11px] font-bold text-(--text-secondary) hover:bg-(--bg-hover) hover:text-white transition"
                    >
                        <ExternalLink className="h-3 w-3" /> 新窗口打开
                    </a>
                    <button
                        onClick={() => { setReady(false); setKey((k) => k + 1); }}
                        className="inline-flex items-center gap-1.5 border border-(--border-default) bg-(--bg-elevated) px-3 py-1.5 text-[11px] font-bold text-(--text-secondary) hover:bg-(--bg-hover) hover:text-white transition"
                    >
                        <RefreshCw className="h-3 w-3" /> 刷新
                    </button>
                </div>
            </header>
            <div className="relative flex-1 bg-white">
                {!ready && (
                    <div className="absolute inset-0 z-10 flex items-center justify-center bg-white text-black">
                        <span className="inline-flex items-center gap-2 text-xs font-bold uppercase tracking-widest">
                            <Loader2 className="h-4 w-4 animate-spin" /> 正在加载 Jaeger UI
                        </span>
                    </div>
                )}
                <iframe
                    key={key}
                    src="/jaeger/search"
                    title="Jaeger 链路追踪"
                    className="w-full h-full border-0"
                    onLoad={() => setReady(true)}
                />
            </div>
        </div>
    );
}
