import { useEffect, useRef, useState, useCallback } from "react";
import { Loader2, Plus, Save, Download, ZoomIn, ZoomOut, Maximize, Undo2, Redo2, CornerDownRight, Trash2 } from "lucide-react";
import { fetchMindMaps, createMindMap, updateMindMap, deleteMindMap, type MindMapData } from "../api"
import { ConfirmModal, useAIPageContext } from "@kit";

interface MindMapNode {
    data: { text: string; expand?: boolean; customLeft?: number; customTop?: number };
    children?: MindMapNode[];
}

const DEFAULT_DATA: MindMapNode = {
    data: {
        text: "根节点",
        customLeft: 200,
        customTop: 200,
    },
    children: [
        {
            data: { text: "主题 1" },
        },
    ],
};

const MENU_ITEMS: {
    label: string;
    cmd?: string;
    key?: string;
    danger?: boolean;
    sep?: boolean;
}[] = [
    { label: "插入同级节点", cmd: "INSERT_NODE" },
    { label: "插入子级节点", cmd: "INSERT_CHILD_NODE" },
    { label: "插入父节点", cmd: "INSERT_PARENT_NODE" },
    { label: "插入概要", cmd: "ADD_GENERALIZATION" },
    { label: "", sep: true },
    { label: "上移节点", cmd: "UP_NODE" },
    { label: "下移节点", cmd: "DOWN_NODE" },
    { label: "", sep: true },
    { label: "展开所有下级节点", cmd: "EXPAND_ALL" },
    { label: "收起所有下级节点", cmd: "UNEXPAND_ALL" },
    // { label: "", sep: true },
    // { label: "复制节点", cmd: "COPY_NODE" },
    // { label: "剪切节点", cmd: "CUT_NODE" },
    // { label: "粘贴节点", cmd: "PASTE_NODE" },
    { label: "", sep: true },
    { label: "删除节点", key: "DELETE", danger: true },
];

let MindMap: any = null;

// 导图上下文桥：在页面组件树内把 getData/onUpdate/mapId 发布到全局 AI 侧栏
//（侧栏 AISidebar 在另一个组件树，只能读到 useAIPageContext 发布的快照）。
function MindMapBridge({
    getData,
    onUpdateMap,
    mapId,
}: {
    getData: () => string;
    onUpdateMap: (data: any) => void;
    mapId: string;
}) {
    useAIPageContext({
        mindMapJson: getData,
        onUpdateMap,
        mindMapId: mapId,
    });
    return null;
}

export function MindMapPage() {
    const containerRef = useRef<HTMLDivElement>(null);
    const mmRef = useRef<any>(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);
    const [deleteConfirm, setDeleteConfirm] = useState<string | null>(null);
    const [mindMaps, setMindMaps] = useState<MindMapData[]>([]);
    const [currentName, setCurrentName] = useState("untitled");
    const [jsonInput, setJsonInput] = useState("");
    const [showImport, setShowImport] = useState(false);
    const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number } | null>(null);
    const [toast, setToast] = useState<string | null>(null);
    const [layout, setLayout] = useState("mindMap");
    const [theme, setTheme] = useState("dark");

    const initMindMap = useCallback(async () => {
        try {
            const [modRes, themesRes] = await Promise.allSettled([
                import("simple-mind-map"),
                import("simple-mind-map-plugin-themes"),
            ]);
            if (modRes.status === "rejected") throw modRes.reason;
            MindMap = modRes.value.default;
            if (themesRes.status === "fulfilled") {
                themesRes.value.default.init(MindMap);
            }
            const result = await fetchMindMaps();
            setMindMaps(result.mindmaps);
            if (result.mindmaps.length > 0) setCurrentName(result.mindmaps[0].name);
            setLoading(false);
        } catch (err) {
            setError('Failed to load: ' + String(err));
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        initMindMap();
    }, [initMindMap]);

    const createMap = (
        data?: MindMapNode,
        opts?: {
            layout?: string;
            theme?: string;
            viewX?: number;
            viewY?: number;
            scale?: number;
        },
    ) => {
        if (!containerRef.current || !MindMap) return;
        if (mmRef.current) mmRef.current.destroy();
        const l = opts?.layout || layout;
        const t = opts?.theme || theme;
        const mm = new MindMap({
            el: containerRef.current,
            data: data || DEFAULT_DATA,
            layout: l,
            theme: t,
            readonly: false,
        });
        // Right-click context menu on nodes
        mm.on("node_contextmenu", (e: MouseEvent) => {
            setCtxMenu({ x: e.clientX + 10, y: e.clientY + 10 });
        });
        // Hide on click
        mm.on("node_click", () => setCtxMenu(null));
        mm.on("draw_click", () => setCtxMenu(null));
        mm.on("expand_btn_click", () => setCtxMenu(null));
        mmRef.current = mm;
        // Restore the saved view transform (viewX/viewY/scale) if provided
        if (opts?.viewX !== undefined || opts?.viewY !== undefined || opts?.scale !== undefined) {
            try {
                const vx = opts?.viewX ?? 0;
                const vy = opts?.viewY ?? 0;
                const s = opts?.scale ?? 1;
                mm.view.setTransformData({
                    transform: { translateX: vx, translateY: vy, scale: s },
                    state: { scale: s, x: vx, y: vy },
                });
            } catch {}
        }
    };

    // Initial load — run only once so subsequent mindMaps updates don't overwrite unsaved edits
    const mapInitRef = useRef(false);
    useEffect(() => {
        if (loading || mapInitRef.current || !containerRef.current) return;
        mapInitRef.current = true;
        if (mindMaps.length > 0) {
            const target =
                mindMaps.find((m) => m.name === currentName) || mindMaps[0];
            if (target) {
                const l = target.layout || "mindMap";
                const t = target.theme || "dark";
                setCurrentName(target.name);
                setLayout(l);
                setTheme(t);
                let data: MindMapNode = DEFAULT_DATA;
                try {
                    data = JSON.parse(target.data);
                } catch {
                    setError("思维导图数据解析失败，已回退到默认数据");
                }
                createMap(data, {
                    layout: l,
                    theme: t,
                    viewX: target.view_x,
                    viewY: target.view_y,
                    scale: target.scale,
                });
                return;
            }
        }
        createMap(undefined, { layout, theme });
        // 仅首次挂载后初始化一次（mapInitRef 幂等）；layout/theme/currentName 只在首帧读取
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [loading, mindMaps]);

    const exec = (cmd?: string) => {
        if (!mmRef.current || !cmd) return;
        mmRef.current.execCommand(cmd, false);
        setCtxMenu(null);
    };

    const execDelete = (currentOnly: boolean) => {
        if (!mmRef.current) return;
        if (currentOnly) {
            // Remove node but keep children
            try {
                const activeNodes = mmRef.current.renderer.activeNodeList;
                if (activeNodes && activeNodes.length > 0) {
                    const node = activeNodes[0];
                    const parent = node.parent;
                    if (parent && node.children) {
                        const idx = parent.children.indexOf(node);
                        // Move children up to parent
                        for (let i = node.children.length - 1; i >= 0; i--) {
                            parent.children.splice(idx, 0, node.children[i]);
                        }
                        parent.children.splice(
                            parent.children.indexOf(node),
                            1,
                        );
                        mmRef.current.render();
                    }
                }
            } catch {}
        } else {
            mmRef.current.execCommand("REMOVE_NODE");
        }
        setCtxMenu(null);
    };

    const handleExport = () => {
        if (!mmRef.current) return;
        const data = mmRef.current.getData();
        const blob = new Blob([JSON.stringify(data, null, 2)], {
            type: "application/json",
        });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = `${currentName}.json`;
        a.click();
        URL.revokeObjectURL(url);
    };

    const handleSave = async () => {
        if (!mmRef.current) return;
        const jsonData = JSON.stringify(mmRef.current.getData());
        let vx = 0, vy = 0, scale = 1;
        try {
            const td = mmRef.current.view.getTransformData();
            vx = td?.translateX ?? 0;
            vy = td?.translateY ?? 0;
            scale = td?.scale ?? 1;
        } catch {}
        const existing = mindMaps.find((m) => m.name === currentName);
        try {
            if (existing?.id) {
                await updateMindMap(existing.id, { name: currentName, data: jsonData, layout, theme, view_x: vx, view_y: vy, scale });
            } else {
                const res = await createMindMap({ name: currentName, data: jsonData, layout, theme, view_x: vx, view_y: vy, scale });
                // Update local state with new ID
                setMindMaps(prev => prev.map(m => m.name === currentName ? { ...m, id: res.mindmap.id } : m));
            }
            setToast("已保存");
            setTimeout(() => setToast(null), 2000);
        } catch (err) { setToast("保存失败: " + String(err)); setTimeout(() => setToast(null), 2000); }
    };


    const handleLoad = (name: string) => {
        const found = mindMaps.find((m) => m.name === name);
        if (found) {
            const l = found.layout || "mindMap";
            const t = found.theme || "dark";
            setCurrentName(name);
            localStorage.setItem("mindmap-current", name);
            setLayout(l);
            setTheme(t);
            let data: MindMapNode = DEFAULT_DATA;
            try {
                data = JSON.parse(found.data);
            } catch {
                setError("思维导图数据解析失败，已回退到默认数据");
            }
            createMap(data, {
                layout: l,
                theme: t,
                viewX: found.view_x,
                viewY: found.view_y,
                scale: found.scale,
            });
        }
    };

    const handleImport = () => {
        try {
            const data = JSON.parse(jsonInput);
            createMap(data);
            setShowImport(false);
            setJsonInput("");
        } catch {
            setError("Invalid JSON");
        }
    };

    const handleNew = async () => {
        const name = prompt("思维导图名称:", `mindmap-${mindMaps.length + 1}`);
        if (!name) return;
        try {
            const data = JSON.stringify(DEFAULT_DATA);
            await createMindMap({ name, data, layout, theme });
            setCurrentName(name);
            const result = await fetchMindMaps();
            setMindMaps(result.mindmaps);
            createMap(DEFAULT_DATA, { layout, theme });
        } catch (err) { setError(String(err)); }
    };

    const handleDeleteItem = async () => {
        const name = deleteConfirm;
        if (!name) return;
        setDeleteConfirm(null);
        const found = mindMaps.find(m => m.name === name);
        if (found?.id) await deleteMindMap(found.id);
        const result = await fetchMindMaps();
        setMindMaps(result.mindmaps);
        if (currentName === name) {
            const next = result.mindmaps[0];
            if (next) { setCurrentName(next.name); handleLoad(next.name); }
            else { setCurrentName("untitled"); createMap(); }
        }
    };

    const handleAddChild = () => {
        if (mmRef.current)
            mmRef.current.execCommand("INSERT_CHILD_NODE", false);
    };
    const handleAddSibling = () => {
        if (mmRef.current) mmRef.current.execCommand("INSERT_NODE", false);
    };
    const handleRemove = () => {
        if (mmRef.current) mmRef.current.execCommand("REMOVE_NODE");
    };
    const handleZoomIn = () => {
        if (mmRef.current) mmRef.current.view.enlarge();
    };
    const handleZoomOut = () => {
        if (mmRef.current) mmRef.current.view.narrow();
    };
    const handleZoomReset = () => {
        if (mmRef.current) mmRef.current.view.reset();
    };
    const handleUndo = () => {
        if (mmRef.current) mmRef.current.execCommand("BACK", false);
    };
    const handleRedo = () => {
        if (mmRef.current) mmRef.current.execCommand("FORWARD", false);
    };

    // Ctrl+S to save（回调经 ref 转发：ref 更新放进 effect，避免 render 期写 ref）
    const saveRef = useRef(handleSave);
    useEffect(() => {
        saveRef.current = handleSave;
    });
    useEffect(() => {
        const handler = (e: KeyboardEvent) => {
            if ((e.ctrlKey || e.metaKey) && e.key === "s") {
                e.preventDefault();
                saveRef.current();
            }
        };
        window.addEventListener("keydown", handler);
        return () => window.removeEventListener("keydown", handler);
    }, []);

    if (loading) {
        return (
            <div className="flex-1 flex items-center justify-center">
                <Loader2 className="h-6 w-6 animate-spin text-(--text-muted)" />
            </div>
        );
    }

    return (
        <div className="flex-1 relative overflow-hidden flex flex-row">
            {/* 画布区（相对定位容器）：工具栏/侧栏/弹窗都覆盖在画布上；右侧 AI 助手 dock 在它旁边 */}
            <div className="flex-1 relative min-w-0">
            {/* Canvas - full screen */}
            <div ref={containerRef} className="absolute inset-0" />

            {/* Floating sidebar */}
            <div className="absolute top-28 left-1 z-10 bg-[#000000]/95 backdrop-blur border border-white/10 rounded-lg w-48 max-h-[60vh] overflow-y-auto py-1">
                <button
                    onClick={handleNew}
                    className="w-full text-left px-3 py-1.5 text-xs text-(--text-secondary) hover:bg-(--bg-hover) flex items-center gap-1"
                >
                    <Plus className="h-3 w-3" /> 新建
                </button>
                <div className="border-t border-white/10 my-1" />
                {mindMaps.map((m) => (
                    <div
                        key={m.name}
                        onClick={() => handleLoad(m.name)}
                        className={`flex items-center justify-between px-3 py-1 text-xs cursor-pointer ${currentName === m.name ? "text-white bg-white/5" : "text-(--text-secondary) hover:bg-(--bg-hover)"}`}
                    >
                        <span className="truncate flex-1">{m.name}</span>
                        <button
                            onClick={(e) => {
                                                            e.stopPropagation();
                                                            setDeleteConfirm(m.name);
                                                        }}
                            className="text-(--text-muted) hover:text-red-400 text-lg"
                        >
                            &times;
                        </button>
                    </div>
                ))}
            </div>

            {/* Floating toolbar - bottom center */}
            <div className="absolute top-3 left-1/2 -translate-x-1/2 z-10 flex items-center gap-2 bg-black/80 backdrop-blur border border-white/10 rounded-lg px-4 py-2">
                <button
                    onClick={handleAddChild}
                    title="子节点 (Tab)"
                    className="p-1.5 text-white/60 hover:text-white leading-none"
                >
                    <Plus className="h-3.5 w-3.5" />
                </button>
                <span className="text-[10px] text-white/30">子</span>
                <button
                    onClick={handleAddSibling}
                    title="同级 (Enter)"
                    className="p-1.5 text-white/60 hover:text-white leading-none"
                >
                    <CornerDownRight className="h-3.5 w-3.5" />
                </button>
                <span className="text-[10px] text-white/30">兄</span>
                <button
                    onClick={handleRemove}
                    title="删除 (Delete)"
                    className="p-1.5 text-white/40 hover:text-red-400 leading-none"
                >
                    <Trash2 className="h-3.5 w-3.5" />
                </button>
                <span className="text-white/10 mx-1">|</span>
                <button
                    onClick={handleUndo}
                    title="撤销"
                    className="p-1.5 text-(--text-secondary) hover:text-white"
                >
                    <Undo2 className="h-3.5 w-3.5" />
                </button>
                <button
                    onClick={handleRedo}
                    title="重做"
                    className="p-1.5 text-(--text-secondary) hover:text-white"
                >
                    <Redo2 className="h-3.5 w-3.5" />
                </button>
                <span className="text-white/10 mx-1">|</span>
                <button
                    onClick={handleZoomOut}
                    className="p-1.5 text-(--text-secondary) hover:text-white"
                >
                    <ZoomOut className="h-3.5 w-3.5" />
                </button>
                <button
                    onClick={handleZoomReset}
                    className="p-1.5 text-(--text-secondary) hover:text-white"
                >
                    <Maximize className="h-3.5 w-3.5" />
                </button>
                <button
                    onClick={handleZoomIn}
                    className="p-1.5 text-(--text-secondary) hover:text-white"
                >
                    <ZoomIn className="h-3.5 w-3.5" />
                </button>
                <span className="text-white/10 mx-1">|</span>
                <select
                    value={theme}
                    onChange={(e) => {
                        const t = e.target.value;
                        setTheme(t);
                        mmRef.current?.setTheme?.(t);
                    }}
                    className="bg-transparent border border-white/10 text-xs text-(--text-secondary) px-2 py-1 rounded"
                >
                    {[
                        "dark",
                        "dark2",
                        "dark3",
                        "dark4",
                        "dark5",
                        "dark6",
                        "dark7",
                    ].map((t) => (
                        <option key={t} value={t}>
                            {t}
                        </option>
                    ))}
                </select>
                <select
                    value={layout}
                    onChange={(e) => {
                        const l = e.target.value;
                        setLayout(l);
                        if (mmRef.current) {
                            const d = mmRef.current.getData();
                            createMap(d, { layout: l, theme });
                        }
                    }}
                    className="bg-transparent border border-white/10 text-xs text-(--text-secondary) px-2 py-1 rounded"
                >
                    {[
                        "mindMap",
                        "logicalStructure",
                        "organizationStructure",
                        "catalogOrganization",
                        "timeline",
                        "timeline2",
                        "fishbone",
                    ].map((l) => (
                        <option key={l} value={l}>
                            {l}
                        </option>
                    ))}
                </select>
                <span className="text-white/10 mx-1">|</span>
                <button
                    onClick={handleExport}
                    className="p-1.5 text-(--text-secondary) hover:text-white"
                >
                    <Download className="h-3.5 w-3.5" />
                </button>
                <button
                    onClick={handleSave}
                    className="p-1.5 text-(--text-secondary) hover:text-white"
                >
                    <Save className="h-3.5 w-3.5" />
                </button>
            </div>

            {/* Import overlay */}
            {showImport && (
                <>
                    <div
                        className="absolute inset-0 z-30 bg-black/60"
                        onClick={() => setShowImport(false)}
                    />
                    <div className="absolute bottom-20 left-1/2 -translate-x-1/2 z-40 bg-(--bg-elevated) border border-(--border-subtle) rounded-lg p-4 w-96">
                        <textarea
                            value={jsonInput}
                            onChange={(e) => setJsonInput(e.target.value)}
                            placeholder="粘贴 JSON..."
                            rows={4}
                            className="w-full bg-black border border-(--border-subtle) px-3 py-2 text-xs focus:outline-none focus:border-white font-mono resize-none"
                        />
                        <div className="flex gap-2 justify-end mt-2">
                            <button
                                onClick={() => setShowImport(false)}
                                className="border border-(--border-subtle) text-(--text-secondary) px-3 py-1 text-xs"
                            >
                                取消
                            </button>
                            <button
                                onClick={handleImport}
                                className="bg-(--accent) text-(--accent-foreground) font-bold px-3 py-1 text-xs"
                            >
                                导入
                            </button>
                        </div>
                    </div>
                </>
            )}

            {error && <div className="absolute top-3 right-3 z-20 border border-(--danger)/50 bg-(--danger)/10 p-2 text-xs text-(--danger)">{error}</div>}
            {toast && <div className="absolute top-3 left-1/2 -translate-x-1/2 z-30 bg-black/90 border border-white/10 rounded px-4 py-2 text-xs text-white">{toast}</div>}
            </div>

            {/* 右侧常驻 AI 助手已升级为全局右侧边栏（AISidebar）：本页面只发布导图上下文 */}
            <MindMapBridge
                getData={() => JSON.stringify(mmRef.current?.getData() || DEFAULT_DATA)}
                onUpdateMap={(data: any) => {
                    createMap(data, { layout, theme });
                    setToast("AI 已更新思维导图");
                    setTimeout(() => setToast(null), 2000);
                }}
                mapId={currentName}
            />

            {/* Context menu */}
            {ctxMenu && (
                <>
                    <div
                        className="fixed inset-0 z-40"
                        onClick={() => setCtxMenu(null)}
                    />
                    <div
                        className="fixed z-50 bg-(--bg-elevated) border border-(--border-subtle) rounded shadow-lg py-1 min-w-[180px]"
                        style={{ left: ctxMenu.x, top: ctxMenu.y }}
                    >
                        {MENU_ITEMS.map((item, i) => {
                            if (item.sep)
                                return (
                                    <div
                                        key={i}
                                        className="border-t border-(--border-subtle) my-1"
                                    />
                                );
                            return (
                                <button
                                    key={i}
                                    onClick={() =>
                                        item.cmd
                                            ? exec(item.cmd)
                                            : item.key === "DELETE"
                                              ? execDelete(false)
                                              : item.key === "DELETE_CURRENT"
                                                ? execDelete(true)
                                                : null
                                    }
                                    className={`w-full text-left px-3 py-1.5 text-xs transition-colors ${item.danger ? "text-red-400 hover:bg-(--bg-hover)" : "text-(--text-secondary) hover:bg-(--bg-hover) hover:text-white"}`}
                                >
                                    {item.label}
                                </button>
                            );
                        })}
                    </div>
                </>
            )}

      {deleteConfirm && (
        <ConfirmModal
          open={true}
          title="删除导图"
          message={'确认删除 "' + (deleteConfirm || '') + '"？'}
          onConfirm={handleDeleteItem}
          onCancel={() => setDeleteConfirm(null)}
        />
      )}
</div>
    );
}
