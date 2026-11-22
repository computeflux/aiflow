import { Routes, Route } from "react-router-dom";
import { Mail, FileText, Server } from "lucide-react";
import { AuthProvider, Sidebar, AISidebar, type NavItem } from "@kit";
import { SsoGate } from "./sso";
import { EmailPage } from "./pages/EmailPage";
import { EmailTemplates } from "./pages/EmailTemplates";
import { EmailSenders } from "./pages/EmailSenders";

const navItems: NavItem[] = [
    { path: "/", label: "邮件推广", icon: Mail },
    { path: "/email-templates", label: "邮件模板", icon: FileText },
    { path: "/email-senders", label: "发信服务商", icon: Server },
];

function Shell() {
    return (
        <div className="flex h-screen text-[#FAFAFA] overflow-hidden" data-ai-sidebar-host>
            <Sidebar items={navItems} />
            <Routes>
                <Route path="/" element={<EmailPage />} />
                <Route
                    path="/email-templates"
                    element={<EmailTemplates />}
                />
                <Route
                    path="/email-senders"
                    element={<EmailSenders />}
                />
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
