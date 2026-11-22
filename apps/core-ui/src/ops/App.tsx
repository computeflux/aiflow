import { Routes, Route, useLocation } from "react-router-dom";
import { AlertTriangle, Activity, Bot, Cpu, FileText, LayoutDashboard, LifeBuoy, Puzzle, ScanSearch, Server } from "lucide-react";
import { AuthProvider as KitAuthProvider, Sidebar, AISidebar, type NavItem } from "@kit";
// ⚠️ 必须显式引枢纽 web3auth 版 AuthProvider（./auth）：@kit 也导出同名
// AuthProvider（会话版），从 @kit 导入会覆盖嵌套结构，导致 /ops/login 的
// useAuth（读 ./auth/context 的 AuthContext）找不到 provider →
// 「useAuth must be used within AuthProvider」。
import { AuthProvider } from "./auth";
import { Console } from "./pages/Console";
import { Login } from "./pages/Login";
import { Errors } from "./pages/Errors";
import { OpsReports } from "./pages/OpsReports";
import { Tracing } from "./pages/Tracing";
import { Interventions } from "./pages/Interventions";
import { Agents } from "./pages/Agents";
import { Skills } from "./pages/Skills";
import { Users } from "./pages/Users";
import { Services } from "./pages/Services";
import { OcrReviews } from "./pages/OcrReviews";
import { Models } from "./pages/Models";
import { OcrReviewDetail } from "./pages/OcrReviewDetail";

// 2026-10-19：工作项中心 / 审批中心 已移到「产品与任务看板」（/product/work-items 、/product/approvals）——
// 任务全生命周期（看板 → 工作项 → 审批）在同一板块，运维中心只留运维性页面。
const navItems: NavItem[] = [
    { path: "/", label: "任务控制台", icon: LayoutDashboard },
    { path: "/services", label: "运行服务", icon: Server },
    { path: "/skills", label: "技能中心", icon: Puzzle },
    { path: "/interventions", label: "干预舱", icon: LifeBuoy },
    { path: "/agents", label: "Agent 管理", icon: Bot },
    { path: "/models", label: "模型管理", icon: Cpu },
    { path: "/ocr-reviews", label: "OCR 审核", icon: ScanSearch },
    { path: "/errors", label: "错误日志", icon: AlertTriangle },
    { path: "/ops-reports", label: "运维情报", icon: FileText },
    { path: "/tracing", label: "链路追踪", icon: Activity },
];

function Shell() {
    // /ops/login 是独立全屏页（唯一登录入口，见 pages/Login.tsx），不套 Sidebar
    const location = useLocation();
    if (location.pathname === "/ops/login") return <Login />;
    return (
        <div className="flex h-screen text-[#FAFAFA] overflow-hidden" data-ai-sidebar-host>
            {/* 用户管理入口仅本服务（运维中心）展示；其他 board 不传 showUsers */}
            <Sidebar items={navItems} showUsers />
            <Routes>
                <Route path="/" element={<Console />} />
                <Route path="/services" element={<Services />} />
                <Route path="/errors" element={<Errors />} />
                <Route path="/ops-reports" element={<OpsReports />} />
                <Route path="/tracing" element={<Tracing />} />
                <Route path="/interventions" element={<Interventions />} />
                <Route path="/agents" element={<Agents />} />
                <Route path="/models" element={<Models />} />
                <Route path="/ocr-reviews" element={<OcrReviews />} />
                <Route path="/ocr-reviews/:taskNo" element={<OcrReviewDetail />} />
                <Route path="/skills" element={<Skills />} />
                <Route path="/users" element={<Users />} />
            </Routes>
            {/* 全局右侧 AI 侧栏（flex 行末尾常驻；登录页等无侧栏页面不经过这里） */}
            <AISidebar />
        </div>
    );
}

export default function App() {
    return (
        <AuthProvider>
            {/* board-kit 的 Sidebar 消费的是 board-kit 自己的 AuthContext（会话版），
                与本页的 web3auth 版 AuthProvider 是两套独立 context；
                嵌套挂载会话版 provider 供 board-kit 组件使用（读同一份 localStorage 会话）。 */}
            <KitAuthProvider>
                <Shell />
            </KitAuthProvider>
        </AuthProvider>
    );
}
