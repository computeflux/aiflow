import React, { useState } from "react";
import { Loader2, RefreshCw, Search, Box, ExternalLink } from "lucide-react";
import { fetchVectorPoints, searchVectorDB, fetchStatus, type VectorPoint, type SearchResultItem, type SearchResponse, type SystemStatus } from "../api";
import { StatsHeader } from "@kit";
import { useAsync } from "../hooks";

export function VectorDB() {
  const [searchResults, setSearchResults] = useState<SearchResultItem[]>([]);
  const [searchResponse, setSearchResponse] = useState<SearchResponse | null>(null);
  const [searchQuery, setSearchQuery] = useState("");
  const [isSearching, setIsSearching] = useState(false);
  const [searchError, setSearchError] = useState<string | null>(null);

  // 部分成功也要展示：两路 allSettled，子请求失败原因并入返回值
  const { data, loading, error, reload: load } = useAsync<{
    points: VectorPoint[];
    statusData: SystemStatus | null;
    partialError: string | null;
  }>(async () => {
    const [dRes, sRes] = await Promise.allSettled([fetchVectorPoints(), fetchStatus()]);
    const errors: string[] = [];
    for (const r of [dRes, sRes]) {
      if (r.status === "rejected") errors.push(r.reason instanceof Error ? r.reason.message : String(r.reason));
    }
    return {
      points: dRes.status === "fulfilled" ? dRes.value : [],
      statusData: sRes.status === "fulfilled" ? sRes.value : null,
      partialError: errors.length > 0 ? errors.join("；") : null,
    };
  }, []);
  const points = data?.points ?? [];
  const statusData = data?.statusData ?? null;

  const handleSearch = async (e: React.FormEvent) => { e.preventDefault(); if(!searchQuery.trim()){setSearchResults([]);setSearchResponse(null);return} setIsSearching(true);setSearchError(null); try { const r=await searchVectorDB(searchQuery.trim()); setSearchResponse(r); setSearchResults(r.documents); } catch(e){ setSearchError(e instanceof Error?e.message:String(e)); } finally { setIsSearching(false); } };

  return (
    <div className="flex-1 p-4 overflow-y-auto">
      <header className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-2xl font-bold tracking-tighter uppercase">Vector Database (Qdrant)</h1>
          <p className="mt-1 text-sm text-(--text-muted)">基于 CRAG 的向量检索与 AI 知识问答</p>
        </div>
        <button onClick={load} disabled={loading} className="border border-(--border-subtle) bg-(--bg-elevated) px-4 py-2 text-xs font-bold text-(--text-secondary) hover:bg-(--bg-hover) transition flex items-center gap-2">
          {loading ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />} REFRESH
        </button>
      </header>
      {(error || data?.partialError || searchError) && <div className="mb-4 border border-(--border-subtle) p-3 text-sm font-bold bg-(--bg-elevated) text-red-500">{error ?? data?.partialError ?? searchError}</div>}
      <StatsHeader statusData={statusData} />
      <div className="mb-8 border border-(--border-subtle) ">
        <div className="p-4 border-b border-(--border-subtle)"><h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted) flex items-center gap-2"><Search className="h-4 w-4" /> CRAG 检索</h2></div>
        <form onSubmit={handleSearch} className="p-4 flex gap-3">
          <input type="text" value={searchQuery} onChange={e => setSearchQuery(e.target.value)} placeholder="输入查询关键词..." className="flex-1 bg-(--bg-elevated) border border-(--border-subtle) px-4 py-2 text-sm focus:outline-none focus:border-white text-white font-medium" />
          <button type="submit" disabled={isSearching} className="border border-(--border-subtle) bg-(--accent) text-(--accent-foreground) font-bold px-6 py-2 text-sm hover:bg-(--text-primary) transition disabled:opacity-50 flex items-center gap-2">{isSearching ? <Loader2 className="h-4 w-4 animate-spin" /> : "检索"}</button>
        </form>
        {searchResponse?.answer && (
          <div className="mx-4 mb-4 border border-(--success) bg-(--success)/10 p-4 text-sm">
            <div className="flex items-center gap-2 mb-2">
              <span className="font-bold text-(--success) uppercase text-xs">AI 综合回答</span>
              {searchResponse.sufficient ? <span className="text-(--success) text-[10px]">● 知识库充足</span> : <span className="text-(--warning) text-[10px]">● 知识库不足</span>}
            </div>
            <p className="text-(--text-primary) leading-relaxed whitespace-pre-wrap">{searchResponse.answer}</p>
          </div>
        )}
        {searchResults.length > 0 && (
          <div className="space-y-3 p-4 pt-0">
            <div className="text-(--text-muted) text-[10px] uppercase font-bold mb-2">相关文档 ({searchResults.length})</div>
            {searchResults.map((item, idx) => (
              <div key={idx} className="border border-(--border-subtle) bg-(--bg-surface) p-4 text-xs">
                <div className="flex justify-between items-center mb-2">
                  <span className="font-bold text-white uppercase border border-(--border-subtle) px-1.5 py-0.5 bg-(--bg-elevated)">MATCH {idx + 1}</span>
                  <span className="font-mono text-(--success) font-bold">Score: {item.score.toFixed(4)}</span>
                </div>
                <p className="text-(--text-primary) leading-relaxed whitespace-pre-wrap">{item.payload?.content || "-"}</p>
                <div className="mt-2 flex items-center gap-3">
                  {item.payload?.source && <span className="text-(--text-muted) font-mono uppercase text-[10px]">来源: {item.payload.source}</span>}
                  {item.payload?.keyword && <span className="text-(--text-muted) font-mono uppercase text-[10px]">关键词: {item.payload.keyword}</span>}
                  {item.payload?.url && <a href={item.payload.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-(--text-secondary) hover:text-white transition-colors text-[10px] font-bold uppercase"><ExternalLink className="h-3 w-3" /> 查看原文</a>}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
      <div className="border border-(--border-subtle) ">
        <div className="p-4 border-b border-(--border-subtle)"><h2 className="text-xs font-bold uppercase tracking-widest text-(--text-muted) flex items-center gap-2"><Box className="h-4 w-4" /> 向量空间点集明细 (Top 50)</h2></div>
        <table className="w-full text-left text-sm">
          <thead className="border-b border-(--border-subtle) text-(--text-muted)"><tr><th className="px-4 py-3 font-bold uppercase w-50">ID</th><th className="px-4 py-3 font-bold uppercase w-32">来源</th><th className="px-4 py-3 font-bold uppercase w-24">关键词</th><th className="px-4 py-3 font-bold uppercase">已索引向量 Payload 内容段</th><th className="px-4 py-3 font-bold uppercase w-16 text-right">链接</th></tr></thead>
          <tbody className="divide-y divide-(--border-divider)">
            {points.length === 0 ? <tr><td colSpan={5} className="px-4 py-8 text-center text-(--text-muted) font-mono text-xs">NO VECTOR POINTS STORED</td></tr> : points.map((point) => (
              <tr key={point.id} className="hover:bg-(--bg-elevated) transition-colors">
                <td className="px-4 py-3 font-mono text-(--text-secondary)">{point.id}</td>
                <td className="px-4 py-3 uppercase font-bold text-xs"><span className="px-1.5 py-0.5 border border-(--border-subtle) bg-(--bg-elevated)">{point.payload?.source || "unknown"}</span></td>
                <td className="px-4 py-3 text-xs text-(--text-secondary)">{point.payload?.keyword || "-"}</td>
                <td className="px-4 py-3"><p className="text-(--text-primary) line-clamp-2 text-xs leading-relaxed" title={point.payload?.content}>{point.payload?.content || "-"}</p></td>
                <td className="px-4 py-3 text-right">{point.payload?.url ? <a href={point.payload.url} target="_blank" rel="noreferrer" className="inline-flex items-center text-(--text-secondary) hover:text-white transition-colors"><ExternalLink className="h-3.5 w-3.5" /></a> : <span className="text-(--text-disabled)">-</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
