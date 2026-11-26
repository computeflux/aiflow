import { Routes, Route } from "react-router-dom";
import { Box, Clock, Database, Heart, KeyRound, ListTodo, Sparkles } from "lucide-react";
import { AuthProvider, Sidebar, type NavItem } from "@kit";
import { SsoGate } from "./sso";
import { SocialPage } from "./pages/SocialPage";
import { XManagePage } from "./pages/XManage";
import { AccountsPage } from "./pages/Accounts";
import { IngestTasksPage } from "./pages/IngestTasks";
import { RawItems } from "./pages/RawItems";
import { VectorDB } from "./pages/VectorDB";
import { Workflows } from "./pages/Workflows";

const navItems: NavItem[] = [
    { path: "/", label: "社媒生成", icon: Sparkles },
    { path: "/x-manage", label: "X 点赞管理", icon: Heart },
    // M1-4：social 渠道账号池管理（渠道 tab / ?channel= 过滤 + 状态徽标 + 触发登录）
    { path: "/accounts", label: "账号管理", icon: KeyRound },
    { path: "/ingest-tasks", label: "定时抓取", icon: Clock },
    { path: "/raw-items", label: "抓取素材库", icon: Database },
    { path: "/vector-db", label: "向量数据库", icon: Box },
    { path: "/workflows", label: "工作流历史", icon: ListTodo },
];

function Shell() {
    return (
        <div className="flex h-screen text-[#FAFAFA] overflow-hidden">
            <Sidebar items={navItems} />
            <Routes>
                <Route path="/" element={<SocialPage />} />
                <Route path="/social" element={<SocialPage />} />
                <Route path="/x-manage" element={<XManagePage />} />
                <Route path="/accounts" element={<AccountsPage />} />
                <Route path="/ingest-tasks" element={<IngestTasksPage />} />
                <Route path="/raw-items" element={<RawItems />} />
                <Route path="/vector-db" element={<VectorDB />} />
                <Route path="/workflows" element={<Workflows />} />
            </Routes>
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
