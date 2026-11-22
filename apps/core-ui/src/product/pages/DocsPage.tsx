import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { marked } from "marked";
import {
    ChevronDown, ChevronRight, Eye, FileCode2, FilePlus, FileText, Folder, FolderOpen,
    FolderPlus, Image as ImageIcon, Loader2, Pencil, RefreshCw, Save, Sparkles, Trash2,
    LayoutList, X,
} from "lucide-react";
import {
    aiDoc, createDocNode, deleteDocNode, fetchBoard, fetchBoards, fetchDocFile, fetchDocRawURL,
    fetchDocTree, renameDocNode, saveDocFile, useAIPageContext, requestAIPanelOpen,
    type Board, type DocFile, type DocNode,
} from "../api";
import { fmtTime } from "../utils";

// 产品「文档」：仓库 docs/ 目录的树形浏览 + 编辑 + 预览（md/html/图片）+ AI 生成 + 拆解为任务。
// 红线在后端（internal/docstore）：路径越界/软链逃逸/二进制写/超大文件都会被拒。
// 预览一律走 sandbox iframe（不给 allow-same-origin）→ 文档里的脚本碰不到应用会话与 token。

const mdCss = `
:root{color-scheme:dark}
body{margin:0;padding:24px 28px;background:#0a0a0a;color:#e5e5e5;font:14px/1.7 -apple-system,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;word-wrap:break-word}
h1,h2,h3,h4{line-height:1.3;margin:1.4em 0 .6em}
h1{font-size:1.7em;border-bottom:1px solid #262626;padding-bottom:.3em}
h2{font-size:1.35em;border-bottom:1px solid #1f1f1f;padding-bottom:.25em}
h3{font-size:1.15em}
a{color:#7dd3fc}
code{background:#171717;padding:.15em .35em;border-radius:4px;font-family:ui-monospace,SFMono-Regular,monospace;font-size:.92em}
pre{background:#141414;padding:12px 14px;border-radius:6px;overflow:auto;border:1px solid #262626}
pre code{background:none;padding:0}
blockquote{margin:1em 0;padding:.2em 1em;border-left:3px solid #404040;color:#a3a3a3}
table{border-collapse:collapse;margin:1em 0}
th,td{border:1px solid #262626;padding:6px 10px}
th{background:#171717}
img{max-width:100%}
hr{border:none;border-top:1px solid #262626;margin:1.6em 0}
ul,ol{padding-left:1.4em}
`;
const previewScript = `document.addEventListener('click',function(e){var a=e.target&&e.target.closest?e.target.closest('a'):null;if(!a)return;var d=a.getAttribute('data-doc');if(d){e.preventDefault();parent.postMessage({type:'docs-open',ref:d,dir:'__DOCDIR__'},'*');}});`;

function esc(s: string): string {
    return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}
function attr(s: string): string {
    return s.replace(/"/g, "%22");
}
// dirOf 取相对路径的目录（"a/b.md" → "a"；"a.md" → ""）。
function dirOf(p: string): string {
    const i = p.lastIndexOf("/");
    return i < 0 ? "" : p.slice(0, i);
}
// joinPath 解析相对引用（处理 ./ 与 ../），越界（跑出 docs 根）返回 ""。
function joinPath(dir: string, ref: string): string {
    const parts = (dir ? dir.split("/") : []).concat(ref.split("/"));
    const out: string[] = [];
    for (const seg of parts) {
        if (seg === "" || seg === ".") continue;
        if (seg === "..") {
            if (out.length === 0) return "";
            out.pop();
            continue;
        }
        out.push(seg);
    }
    return out.join("/");
}
function humanSize(n: number): string {
    if (n < 1024) return `${n} B`;
    if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
    return `${(n / 1024 / 1024).toFixed(2)} MB`;
}
const IMG_EXT = /\.(png|jpe?g|gif|webp|svg|ico|bmp)$/i;
const MD_EXT = /\.(md|markdown)$/i;
const HTML_EXT = /\.(html?|htm)$/i;

function fileIcon(name: string) {
    if (IMG_EXT.test(name)) return <ImageIcon className="h-3.5 w-3.5 shrink-0 text-(--warning)" />;
    if (MD_EXT.test(name)) return <FileText className="h-3.5 w-3.5 shrink-0 text-(--info)" />;
    return <FileCode2 className="h-3.5 w-3.5 shrink-0 text-(--text-muted)" />;
}

// rewriteRefs 把 markdown 渲染出的相对引用改写到签名 raw 前缀（图片/CSS 能加载）；
// 站内 .md 链接改成 data-doc，由 iframe 内脚本 postMessage 回父窗口在编辑器里打开。
function rewriteRefs(html: string, dirBase: string, docDir: string): string {
    return html.replace(/(src|href)="([^"]+)"/g, (whole, tag: string, ref: string) => {
        if (/^(https?:)?\/\//.test(ref) || ref.startsWith("data:") || ref.startsWith("#") || ref.startsWith("mailto:")) {
            return whole;
        }
        const clean = ref.replace(/^\.\//, "");
        if (MD_EXT.test(clean)) {
            return `href="#" data-doc="${attr(clean)}"`;
        }
        void docDir;
        return `${tag}="${attr(`${dirBase}/${clean}`)}"`;
    });
}

function mdToPreviewHTML(md: string, dirBase: string, docDir: string): string {
    const body = rewriteRefs(marked.parse(md, { async: false }) as string, dirBase, docDir);
    const script = previewScript.replace("__DOCDIR__", docDir.replace(/'/g, ""));
    return `<!doctype html><html><head><meta charset="utf-8"><style>${mdCss}</style></head><body>${body}<script>${script}</script></body></html>`;
}

function textToPreviewHTML(text: string): string {
    return `<!doctype html><html><head><meta charset="utf-8"><style>${mdCss}pre{border:none;background:none;padding:0}</style></head><body><pre>${esc(text)}</pre></body></html>`;
}

type Preview =
    | { kind: "srcdoc"; html: string }
    | { kind: "src"; url: string }
    | { kind: "img"; url: string };

// ---- 目录树节点 ----
function TreeItem({
    node, depth, expanded, toggle, currentPath, onOpen,
}: {
    node: DocNode;
    depth: number;
    expanded: Set<string>;
    toggle: (p: string) => void;
    currentPath: string;
    onOpen: (p: string) => void;
}) {
    const pad = { paddingLeft: `${depth * 12 + 8}px` };
    if (node.type === "file") {
        const active = currentPath === node.path;
        return (
            <button
                onClick={() => onOpen(node.path)}
                style={pad}
                title={node.path}
                className={`w-full flex items-center gap-1.5 py-1 pr-2 text-left text-xs hover:bg-(--bg-hover) ${
                    active ? "bg-(--bg-elevated) text-white" : "text-(--text-secondary)"
                }`}
            >
                <span className="w-3 shrink-0" />
                {fileIcon(node.name)}
                <span className="truncate">{node.name}</span>
            </button>
        );
    }
    const open = expanded.has(node.path);
    return (
        <div>
            <button
                onClick={() => toggle(node.path)}
                style={pad}
                className="w-full flex items-center gap-1.5 py-1 pr-2 text-left text-xs text-(--text-secondary) hover:bg-(--bg-hover)"
            >
                {open ? <ChevronDown className="h-3 w-3 shrink-0" /> : <ChevronRight className="h-3 w-3 shrink-0" />}
                {open ? <FolderOpen className="h-3.5 w-3.5 shrink-0 text-(--warning)" /> : <Folder className="h-3.5 w-3.5 shrink-0 text-(--warning)" />}
                <span className="truncate font-medium">{node.name}</span>
            </button>
            {open &&
                (node.children ?? []).map((c) => (
                    <TreeItem key={c.path} node={c} depth={depth + 1} expanded={expanded} toggle={toggle} currentPath={currentPath} onOpen={onOpen} />
                ))}
        </div>
    );
}

// ---- 主页面 ----
export function DocsPage() {
    const [tree, setTree] = useState<DocNode | null>(null);
    const [expanded, setExpanded] = useState<Set<string>>(new Set());
    const [loading, setLoading] = useState(false);
    const [msg, setMsg] = useState<{ kind: "ok" | "err"; text: string } | null>(null);

    const [file, setFile] = useState<DocFile | null>(null);
    const [content, setContent] = useState("");
    const [dirty, setDirty] = useState(false);
    const [mode, setMode] = useState<"edit" | "preview">("edit");
    const [preview, setPreview] = useState<Preview | null>(null);
    const [busy, setBusy] = useState<"save" | "load" | "preview" | null>(null);

    const [aiOpen, setAiOpen] = useState(false);
    const [aiMode, setAiMode] = useState<"generate" | "edit">("generate");
    const [aiPrompt, setAiPrompt] = useState("");
    const [aiTarget, setAiTarget] = useState("");
    const [aiBusy, setAiBusy] = useState(false);

    // 工作看板式 AI 助手（2026-11-01）：和 BoardPage 一样「选中看板就常驻挂载 AIPanel」——
    // 不是「点弹窗 → 一次性拆解」。当前文档作为上下文自动带上，随时可以问、随时可以建卡。
    const [aiBoard, setAiBoard] = useState<Board | null>(null);
    const [aiTargetListId, setAiTargetListId] = useState(0);

    const contentRef = useRef(content);

    // 全局右侧 AI 侧栏：发布「看板 + 当前文档」上下文；工具栏「AI 助手」按钮触发打开
    useAIPageContext({
        board: aiBoard ?? undefined,
        onRefresh: () => {
            if (aiBoard) void fetchBoard(aiBoard.id).then(setAiBoard).catch(() => {});
        },
        doc: file ? { path: `docs/${file.path}`, content } : undefined,
        targetListId: aiTargetListId || undefined,
    });

    // contentRef 只在事件回调（保存/预览/拆解）里取最新正文——这些回调都发生在 render 之后，
    // 故在 effect 里同步（不在 render 期赋 ref，避免 hooks 规则告警）。
    useEffect(() => {
        contentRef.current = content;
    }, [content]);

    // flat：树里全部路径（AI 生成时判断目标是否已存在）
    const flat = useMemo(() => {
        const out: string[] = [];
        const walk = (n: DocNode) => {
            for (const c of n.children ?? []) {
                out.push(c.path);
                if (c.type === "dir") walk(c);
            }
        };
        if (tree) walk(tree);
        return out;
    }, [tree]);

    const loadTree = useCallback(async (keepExpanded = true) => {
        setLoading(true);
        try {
            const t = await fetchDocTree();
            setTree(t);
            setExpanded((prev) => {
                if (keepExpanded && prev.size > 0) return prev;
                return new Set((t.children ?? []).filter((c) => c.type === "dir").map((c) => c.path));
            });
        } catch (e) {
            setMsg({ kind: "err", text: e instanceof Error ? e.message : String(e) });
        } finally {
            setLoading(false);
        }
    }, []);
    // 首帧取数在事件回调外触发（React 官方数据获取模式；loadTree 是稳定引用）
    // eslint-disable-next-line react-hooks/set-state-in-effect
    useEffect(() => { void loadTree(false); }, [loadTree]);

    const toggle = (p: string) =>
        setExpanded((prev) => {
            const next = new Set(prev);
            if (next.has(p)) next.delete(p);
            else next.add(p);
            return next;
        });

    const openFile = useCallback(
        async (path: string) => {
            if (dirty && !window.confirm(`「${file?.path}」有未保存改动，放弃并打开新文档？`)) return;
            setBusy("load");
            setMsg(null);
            try {
                const f = await fetchDocFile(path);
                setFile(f);
                setContent(f.content);
                setDirty(false);
                setPreview(null);
                setMode(f.editable ? "edit" : "preview");
                // 展开所在目录链，便于在树里看到当前文件
                const segs = path.split("/");
                segs.pop();
                const chain: string[] = [];
                let acc = "";
                for (const s of segs) {
                    acc = acc ? `${acc}/${s}` : s;
                    chain.push(acc);
                }
                setExpanded((prev) => new Set([...prev, ...chain]));
            } catch (e) {
                setMsg({ kind: "err", text: e instanceof Error ? e.message : String(e) });
            } finally {
                setBusy(null);
            }
        },
        [dirty, file?.path],
    );

    // 预览构建（md → marked + 签名 raw 前缀；html → iframe 直出 raw；图片 → img）
    const buildPreview = useCallback(
        async (f: DocFile | null, text: string) => {
            if (!f) return;
            setBusy("preview");
            setMsg(null);
            try {
                if (IMG_EXT.test(f.path)) {
                    setPreview({ kind: "img", url: await fetchDocRawURL(f.path) });
                    return;
                }
                if (HTML_EXT.test(f.path)) {
                    setPreview({ kind: "src", url: await fetchDocRawURL(f.path) });
                    return;
                }
                if (MD_EXT.test(f.path)) {
                    const raw = await fetchDocRawURL(f.path);
                    const dirBase = raw.slice(0, raw.lastIndexOf("/"));
                    setPreview({ kind: "srcdoc", html: mdToPreviewHTML(text, dirBase, dirOf(f.path)) });
                    return;
                }
                if (f.binary) {
                    setPreview({ kind: "src", url: await fetchDocRawURL(f.path) });
                    return;
                }
                setPreview({ kind: "srcdoc", html: textToPreviewHTML(text) });
            } catch (e) {
                setMsg({ kind: "err", text: e instanceof Error ? e.message : String(e) });
            } finally {
                setBusy(null);
            }
        },
        [],
    );

    const switchMode = async (m: "edit" | "preview") => {
        setMode(m);
        if (m === "preview") await buildPreview(file, contentRef.current);
    };

    const save = useCallback(async () => {
        if (!file || !dirty) return;
        setBusy("save");
        setMsg(null);
        try {
            const f = await saveDocFile(file.path, contentRef.current);
            setFile(f);
            setDirty(false);
            setMsg({ kind: "ok", text: `已保存 ${f.path}（${humanSize(f.size)}）` });
            if (mode === "preview") await buildPreview(f, f.content);
        } catch (e) {
            setMsg({ kind: "err", text: e instanceof Error ? e.message : String(e) });
        } finally {
            setBusy(null);
        }
    }, [file, dirty, mode, buildPreview]);

    // Ctrl/Cmd+S 保存
    useEffect(() => {
        const h = (e: KeyboardEvent) => {
            if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
                e.preventDefault();
                void save();
            }
        };
        window.addEventListener("keydown", h);
        return () => window.removeEventListener("keydown", h);
    }, [save]);

    // iframe 内点击站内文档链接 → 在编辑器打开
    useEffect(() => {
        const h = (e: MessageEvent) => {
            const d = e.data as { type?: string; ref?: string; dir?: string } | null;
            if (!d || d.type !== "docs-open" || !d.ref) return;
            const p = joinPath(d.dir ?? "", d.ref);
            if (p) void openFile(p);
        };
        window.addEventListener("message", h);
        return () => window.removeEventListener("message", h);
    }, [openFile]);

    const doNew = async (isDir: boolean) => {
        const input = window.prompt(isDir ? "新建目录（相对 docs/）" : "新建文件（相对 docs/，如 plan/new.md）", isDir ? "plan/" : "plan/新文档.md");
        if (!input) return;
        const p = input.trim().replace(/^\/+/, "");
        if (!p) return;
        try {
            await createDocNode(p, isDir);
            await loadTree();
            setMsg({ kind: "ok", text: `已新建 ${p}` });
            if (!isDir) await openFile(p);
        } catch (e) {
            setMsg({ kind: "err", text: e instanceof Error ? e.message : String(e) });
        }
    };

    const doRename = async () => {
        if (!file) return;
        const to = window.prompt("重命名/移动（相对 docs/）", file.path);
        if (!to || to.trim() === file.path) return;
        try {
            await renameDocNode(file.path, to.trim().replace(/^\/+/, ""));
            const next = to.trim().replace(/^\/+/, "");
            await loadTree();
            await openFile(next);
            setMsg({ kind: "ok", text: `已重命名为 ${next}` });
        } catch (e) {
            setMsg({ kind: "err", text: e instanceof Error ? e.message : String(e) });
        }
    };

    const doDelete = async () => {
        if (!file) return;
        if (!window.confirm(`删除 ${file.path}？（不可恢复）`)) return;
        try {
            await deleteDocNode(file.path);
            setFile(null);
            setContent("");
            setDirty(false);
            setPreview(null);
            await loadTree();
            setMsg({ kind: "ok", text: "已删除" });
        } catch (e) {
            setMsg({ kind: "err", text: e instanceof Error ? e.message : String(e) });
        }
    };

    // AI：生成新文档（直接落盘到目标路径）或按指示改写当前文档（结果进编辑器，未保存）
    const runAI = async () => {
        const p = aiPrompt.trim();
        if (!p) return;
        if (aiMode === "edit" && !file) {
            setMsg({ kind: "err", text: "请先在左侧打开要改写的文档" });
            return;
        }
        setAiBusy(true);
        setMsg(null);
        try {
            if (aiMode === "edit") {
                const out = await aiDoc("edit", p, file?.path ?? "");
                setContent(out);
                setDirty(true);
                setMode("edit");
                setMsg({ kind: "ok", text: "AI 已改写正文（未保存）——请审阅后保存" });
            } else {
                const target = (aiTarget.trim() || `plan/ai-${Date.now()}.md`).replace(/^\/+/, "");
                const out = await aiDoc("generate", p, target);
                if (flat.includes(target)) {
                    if (!window.confirm(`${target} 已存在，覆盖？`)) return;
                    await saveDocFile(target, out);
                } else {
                    await createDocNode(target, false);
                    await saveDocFile(target, out);
                }
                await loadTree();
                await openFile(target);
                setMsg({ kind: "ok", text: `AI 已生成并保存 ${target}` });
            }
            setAiPrompt("");
        } catch (e) {
            setMsg({ kind: "err", text: e instanceof Error ? e.message : String(e) });
        } finally {
            setAiBusy(false);
        }
    };

    // 工作看板式助手的依赖：自动选第一个看板，默认落列优先 Backlog
    // （与产品需求「新需求先落 Backlog」一致；没有 Backlog 就退到第一列）。
    useEffect(() => {
        let cancelled = false;
        (async () => {
            try {
                const bs = (await fetchBoards()).boards;
                const first = bs[0];
                if (!first || cancelled) return;
                const b = await fetchBoard(first.id);
                if (cancelled) return;
                setAiBoard(b);
                const lists = b.lists ?? [];
                const backlog = lists.find((l) => l.name === "Backlog") ?? lists[0];
                setAiTargetListId(backlog?.id ?? 0);
            } catch {
                /* 看板不可用时不挂 AI 助手（不影响文档编辑） */
            }
        })();
        return () => {
            cancelled = true;
        };
    }, []);

    const isImg = file ? IMG_EXT.test(file.path) : false;
    const canPreview = file ? !file.binary || isImg || HTML_EXT.test(file.path) : false;

    return (
        <div className="flex-1 relative flex flex-col overflow-hidden">
            {/* 顶栏 */}
            <div className="flex items-center gap-2 border-b border-(--border-subtle) px-4 py-2 shrink-0">
                <span className="text-sm font-bold tracking-wide">文档</span>
                <span className="text-xs text-(--text-muted)">docs/ · 树形浏览 / 编辑 / 预览</span>
                <div className="flex-1" />
                <button onClick={() => void doNew(false)} className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-xs text-(--text-secondary) hover:bg-(--bg-hover)">
                    <FilePlus className="h-3.5 w-3.5" /> 新建文件
                </button>
                <button onClick={() => void doNew(true)} className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-xs text-(--text-secondary) hover:bg-(--bg-hover)">
                    <FolderPlus className="h-3.5 w-3.5" /> 新建目录
                </button>
                <button onClick={() => void loadTree()} className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-xs text-(--text-secondary) hover:bg-(--bg-hover)">
                    {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5" />} 刷新
                </button>
                <button onClick={() => requestAIPanelOpen()} className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-xs text-(--text-secondary) hover:bg-(--bg-hover)">
                    <LayoutList className="h-3.5 w-3.5" /> AI 助手
                </button>
                <button
                    onClick={() => setAiOpen((v) => !v)}
                    className={`flex items-center gap-1 border px-2 py-1 text-xs ${aiOpen ? "border-(--accent) text-(--accent)" : "border-(--border-subtle) bg-(--bg-elevated) text-(--text-secondary) hover:bg-(--bg-hover)"}`}
                >
                    <Sparkles className="h-3.5 w-3.5" /> AI 助手
                </button>
            </div>

            {msg && (
                <div className={`px-4 py-2 text-xs shrink-0 ${msg.kind === "ok" ? "text-(--ok)" : "text-(--danger)"} border-b border-(--border-subtle)`}>
                    {msg.text}
                </div>
            )}

            <div className="flex flex-1 min-h-0">
                {/* 左侧目录树 */}
                <div className="w-64 shrink-0 border-r border-(--border-subtle) overflow-auto py-1">
                    {!tree && loading && (
                        <div className="flex items-center gap-2 px-3 py-2 text-xs text-(--text-muted)">
                            <Loader2 className="h-3.5 w-3.5 animate-spin" /> 加载目录…
                        </div>
                    )}
                    {tree?.children?.map((c) => (
                        <TreeItem key={c.path} node={c} depth={0} expanded={expanded} toggle={toggle} currentPath={file?.path ?? ""} onOpen={(p) => void openFile(p)} />
                    ))}
                </div>

                {/* 编辑 / 预览 */}
                <div className="flex-1 min-w-0 flex flex-col">
                    {!file ? (
                        <div className="flex-1 flex items-center justify-center text-xs text-(--text-muted)">
                            从左侧选择文档，或「新建文件」/「AI 助手」生成新文档
                        </div>
                    ) : (
                        <>
                            <div className="flex items-center gap-2 border-b border-(--border-subtle) px-3 py-1.5 shrink-0">
                                <span className="font-mono text-xs truncate" title={file.path}>
                                    {file.path}
                                    {dirty && <span className="text-(--warning)"> ●</span>}
                                </span>
                                <span className="text-[10px] text-(--text-muted) shrink-0">
                                    {humanSize(file.size)} · {fmtTime(file.mtime)}
                                </span>
                                <div className="flex-1" />
                                <div className="flex items-center border border-(--border-subtle)">
                                    <button
                                        onClick={() => void switchMode("edit")}
                                        disabled={!file.editable}
                                        className={`flex items-center gap-1 px-2 py-1 text-[10px] ${mode === "edit" ? "bg-(--bg-elevated) text-white" : "text-(--text-muted)"} disabled:opacity-40`}
                                    >
                                        <Pencil className="h-3 w-3" /> 编辑
                                    </button>
                                    <button
                                        onClick={() => void switchMode("preview")}
                                        disabled={!canPreview}
                                        className={`flex items-center gap-1 px-2 py-1 text-[10px] ${mode === "preview" ? "bg-(--bg-elevated) text-white" : "text-(--text-muted)"} disabled:opacity-40`}
                                    >
                                        {busy === "preview" ? <Loader2 className="h-3 w-3 animate-spin" /> : <Eye className="h-3 w-3" />} 预览
                                    </button>
                                </div>
                                <button
                                    onClick={() => void save()}
                                    disabled={!dirty || busy === "save"}
                                    className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-[10px] text-(--text-secondary) hover:bg-(--bg-hover) disabled:opacity-40"
                                    title="保存（Ctrl/Cmd+S）"
                                >
                                    {busy === "save" ? <Loader2 className="h-3 w-3 animate-spin" /> : <Save className="h-3 w-3" />} 保存
                                </button>
                                <button onClick={() => void doRename()} className="flex items-center gap-1 border border-(--border-subtle) bg-(--bg-elevated) px-2 py-1 text-[10px] text-(--text-secondary) hover:bg-(--bg-hover)">
                                    <Pencil className="h-3 w-3" /> 重命名
                                </button>
                                <button onClick={() => void doDelete()} className="flex items-center gap-1 border border-(--danger)/50 px-2 py-1 text-[10px] text-(--danger) hover:bg-(--danger) hover:text-black">
                                    <Trash2 className="h-3 w-3" /> 删除
                                </button>
                            </div>

                            <div className="flex-1 min-h-0">
                                {mode === "edit" && file.editable && (
                                    <textarea
                                        value={content}
                                        onChange={(e) => {
                                            setContent(e.target.value);
                                            setDirty(true);
                                        }}
                                        spellCheck={false}
                                        className="w-full h-full resize-none bg-(--bg-base) px-4 py-3 font-mono text-xs leading-relaxed focus:outline-none"
                                    />
                                )}
                                {mode === "edit" && !file.editable && (
                                    <div className="p-4 text-xs text-(--text-muted)">
                                        该文件不可编辑（二进制或超过 2MB 上限）—— 切到「预览」查看。
                                    </div>
                                )}
                                {mode === "preview" && preview?.kind === "srcdoc" && (
                                    <iframe title="preview" sandbox="allow-scripts" srcDoc={preview.html} className="w-full h-full bg-(--bg-base)" />
                                )}
                                {mode === "preview" && preview?.kind === "src" && (
                                    <iframe title="preview" sandbox="allow-scripts allow-popups" src={preview.url} className="w-full h-full bg-white" />
                                )}
                                {mode === "preview" && preview?.kind === "img" && (
                                    <div className="p-4 overflow-auto h-full">
                                        <img src={preview.url} alt={file.path} className="max-w-full" />
                                    </div>
                                )}
                            </div>
                        </>
                    )}
                </div>

                {/* AI 面板 */}
                {aiOpen && (
                    <div className="w-80 shrink-0 border-l border-(--border-subtle) flex flex-col">
                        <div className="flex items-center gap-2 px-3 py-2 border-b border-(--border-subtle)">
                            <Sparkles className="h-3.5 w-3.5 text-(--accent)" />
                            <span className="text-xs font-bold">AI 文档助手</span>
                            <div className="flex-1" />
                            <button onClick={() => setAiOpen(false)} className="text-(--text-muted) hover:text-white">
                                <X className="h-3.5 w-3.5" />
                            </button>
                        </div>
                        <div className="p-3 space-y-2 overflow-auto flex-1">
                            <div className="flex items-center border border-(--border-subtle)">
                                <button
                                    onClick={() => setAiMode("generate")}
                                    className={`flex-1 px-2 py-1 text-[10px] ${aiMode === "generate" ? "bg-(--bg-elevated) text-white" : "text-(--text-muted)"}`}
                                >
                                    生成新文档
                                </button>
                                <button
                                    onClick={() => setAiMode("edit")}
                                    className={`flex-1 px-2 py-1 text-[10px] ${aiMode === "edit" ? "bg-(--bg-elevated) text-white" : "text-(--text-muted)"}`}
                                >
                                    改写当前文档
                                </button>
                            </div>
                            {aiMode === "generate" && (
                                <input
                                    value={aiTarget}
                                    onChange={(e) => setAiTarget(e.target.value)}
                                    placeholder="保存路径（相对 docs/，如 plan/new.md）"
                                    className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1.5 text-xs focus:outline-none focus:border-white font-mono"
                                />
                            )}
                            <textarea
                                value={aiPrompt}
                                onChange={(e) => setAiPrompt(e.target.value)}
                                rows={6}
                                placeholder={aiMode === "generate" ? "描述要生成的文档（写清主题、范围、期望结构）…" : `对「${file?.path ?? "（未打开文档）"}」的修改要求…`}
                                className="w-full bg-(--bg-elevated) border border-(--border-subtle) px-2 py-1.5 text-xs focus:outline-none focus:border-white resize-y"
                            />
                            <button
                                onClick={() => void runAI()}
                                disabled={aiBusy || !aiPrompt.trim()}
                                className="w-full flex items-center justify-center gap-1 bg-(--accent) text-(--accent-foreground) font-bold px-3 py-1.5 text-xs disabled:opacity-40"
                            >
                                {aiBusy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Sparkles className="h-3.5 w-3.5" />}
                                {aiBusy ? "生成中…" : aiMode === "generate" ? "生成并保存" : "改写（结果进编辑器，未保存）"}
                            </button>
                            <div className="text-[10px] leading-relaxed text-(--text-muted)">
                                · 生成时会带上 docs/ 目录清单做上下文，避免重复造轮子
                                <br />· 改写只改编辑器内容，需你确认后保存
                                <br />· 生成的新文档若路径已存在会先确认覆盖
                            </div>
                        </div>
                    </div>
                )}
            </div>

            {/* 常驻 AI 助手已升级为全局右侧边栏（AISidebar）：本页面只发布上下文
                （看板 + 当前文档），工具栏「AI 助手」按钮只负责把侧栏打开。 */}

        </div>
    );
}
