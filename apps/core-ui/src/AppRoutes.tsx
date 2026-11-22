import { lazy, Suspense } from "react";
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import { SsoFrame } from "./kit/sso-frame";
import { ErrorBoundary } from "./kit/components/ErrorBoundary";

// 单入口 React 项目：/ops /product /content /email 四个板块由路由区分，
// 按路由懒加载做代码分割（进 /ops/ 只下载 ops 的 chunk，重依赖互不拖累）。
const OpsApp = lazy(() => import("./ops/App"));
const ProductApp = lazy(() => import("./product/App"));
const ContentApp = lazy(() => import("./content/App"));
const EmailApp = lazy(() => import("./email/App"));

function Loading() {
  return (
    <div className="flex h-screen items-center justify-center text-sm text-[#71717A]">
      加载中…
    </div>
  );
}

export function AppRoutes() {
  return (
    <ErrorBoundary>
      <BrowserRouter>
        <Suspense fallback={<Loading />}>
          <Routes>
            <Route path="/" element={<Navigate to="/ops" replace />} />
            {/* 跨 origin 会话同步端点（空白页，不在 SsoGate 内）：/ops/sso */}
            <Route path="/ops/sso" element={<SsoFrame />} />
            <Route path="/ops/*" element={<OpsApp />} />
            <Route path="/product/*" element={<ProductApp />} />
            <Route path="/content/*" element={<ContentApp />} />
            <Route path="/email/*" element={<EmailApp />} />
            {/* 未匹配任何板块（含旧 /review/ 书签）→ 回运维中心 */}
            <Route path="*" element={<Navigate to="/ops" replace />} />
          </Routes>
        </Suspense>
      </BrowserRouter>
    </ErrorBoundary>
  );
}
