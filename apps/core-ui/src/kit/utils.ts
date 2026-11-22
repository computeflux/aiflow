export function fmtTime(v?: string | number | Date | null): string {
    if (v === null || v === undefined || v === "") return "-";
    const d = new Date(v);
    return isNaN(d.getTime()) ? "-" : d.toLocaleString();
}

// 进化单工作目录的编辑器入口（跳转 code-server，审核时比看截断的 diff 直观）。
// 目录约定：宿主机 <repo>/cache/evolve/<task_no>；code-server 监听 :8088。
const EVOLVE_EDITOR_PORT = 8088;
const EVOLVE_EDITOR_WORKDIR_ROOT = "/home/wetee/work/cathome/workflow/cache/evolve";

export function worktreeEditorUrl(taskNo: string): string {
    if (!taskNo) return "";
    const host = typeof window === "undefined" ? "localhost" : window.location.hostname;
    const folder = encodeURI(`${EVOLVE_EDITOR_WORKDIR_ROOT}/${taskNo}`);
    return `http://${host}:${EVOLVE_EDITOR_PORT}/?folder=${folder}`;
}
