import { Routes, Route } from "react-router-dom";
import { Columns, GitFork, FolderGit2, ListTodo, CheckSquare, FileText } from "lucide-react";
import { AuthProvider, Sidebar, AISidebar } from "@kit";
import { SsoGate } from "./sso";
import { BoardPage } from "./pages/BoardPage";
import { MindMapPage } from "./pages/MindMapPage";
import { Projects } from "./pages/Projects";
import { WorkItems } from "./pages/WorkItems";
import { Approvals } from "./pages/Approvals";
import { DocsPage } from "./pages/DocsPage";

// S9 §7.4 重设：Bug 队列 = 工作看板里的 Bug 看板（复用看板机制：拖 In Progress = 交给工作流）
// 2026-10-19：工作项中心 / 审批中心 从运维中心移入本板块；新增「文档」（仓库 docs/ 浏览编辑）。
// 2026-10-19：去掉「产品情报」页（S7 拆解能力由「文档 → 拆解为任务」与看板 AI 助手承接），
// 本板块默认页 = 工作看板（/product/ 直接进看板）。
const NAV_ITEMS = [
    { path: "/", label: "工作看板", icon: Columns },
    { path: "/work-items", label: "工作项中心", icon: ListTodo },
    { path: "/approvals", label: "审批中心", icon: CheckSquare },
    { path: "/mindmap", label: "思维导图", icon: GitFork },
    { path: "/docs", label: "文档", icon: FileText },
    { path: "/projects", label: "项目", icon: FolderGit2 },
];

function Shell() {
    return (
        <div className="flex h-screen text-[#FAFAFA] overflow-hidden" data-ai-sidebar-host>
            <Sidebar items={NAV_ITEMS} />
            <Routes>
                <Route path="/" element={<BoardPage />} />
                <Route path="/bugs" element={<BoardPage initialBoardName="Bug" />} />
                <Route path="/work-items" element={<WorkItems />} />
                <Route path="/approvals" element={<Approvals />} />
                <Route path="/docs" element={<DocsPage />} />
                <Route path="/projects" element={<Projects />} />
                <Route path="/mindmap" element={<MindMapPage />} />
            </Routes>
            {/* 全局右侧 AI 侧栏（flex 行末尾常驻） */}
            <AISidebar />
        </div>
    );
}

export default function App() {
    return (
        <AuthProvider>
            {/* 未登录 / 401 → 跳枢纽 /ops/login 获取登录凭证（本服务不弹登录框） */}
            <SsoGate>
                <Shell />
            </SsoGate>
        </AuthProvider>
    );
}
