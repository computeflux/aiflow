import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import { consumeSsoOnBoot } from "./kit/sso";
import { setRefreshHandler } from "./kit/api";
import { refreshSession } from "./kit/auth/tokenStore";
import { AppRoutes } from "./AppRoutes";

// 全站（同源四板块）统一静默续期：API 401 → 枢纽单例 refresh 后重放一次。
// 只有枢纽 origin 允许轮换 refresh token（跨 origin 各持一份会触发重放检测 → 吊销会话）；
// 非枢纽服务用 iframe（/ops/sso）由枢纽代续（见 kit/sso-frame.tsx）。
setRefreshHandler(async () => (await refreshSession())?.token ?? null);

// S1 全局右侧 AI 侧栏：每个板块 Shell 的根 div 上标记 data-ai-sidebar-host，
// AISidebar 在 React 树内作为宿主 flex 行的最后一个子元素常驻右侧（登录页等
// 无标记页面不渲染），会话状态跨页保留。
//
// ⚠️ 历史教训（2026-11-20）：绝不要在组件树外移动 React 管理的节点——portal /
// DOM appendChild「搬运」侧栏节点后，宿主重渲染会把搬来的节点当作陌生 DOM
// 移除，React 侧 vnode 还活着 → 树脱钩、context 全丢，表现为子组件抛
// 「useAuth must be used within AuthProvider」。这里只在 <head> 注入一份
// <style>（安全），布局全部交给 CSS + flex。
function ensureSidebarStyle() {
    const AI_SIDEBAR_STYLE_ID = "ai-sidebar-style";
    if (document.getElementById(AI_SIDEBAR_STYLE_ID)) return;
    const style = document.createElement("style");
    style.id = AI_SIDEBAR_STYLE_ID;
    // 侧栏根用 display:contents：布局完全交给 AISidebar 渲染的 flex 子项
    //（展开 w-96 / 收起 w-9），宿主是 flex 行 → 自然成为右列。
    style.textContent = ".ai-sidebar-root { display: contents; }";
    document.head.appendChild(style);
}

// SSO 回跳：URL 带 ?sso=base64 会话时先写入本地 localStorage，再渲染
//（web3auth SDK 挂载时 restoreSession 即可恢复登录态）。
async function boot() {
    ensureSidebarStyle();
    await consumeSsoOnBoot();
    createRoot(document.getElementById("root")!).render(
        <StrictMode>
            <AppRoutes />
        </StrictMode>,
    );
}
void boot();
