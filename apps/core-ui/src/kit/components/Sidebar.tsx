import { Link, useLocation } from "react-router-dom";
import { useEffect, useRef, useState } from "react";
import { Box, Mail, Server, Check, ArrowLeftRight, Workflow, FileText, User as UserIcon, LogIn, LogOut, type LucideIcon } from "lucide-react";
import { useAuth } from "../hooks/useAuth";
import { redirectToLogin } from "../sso";

interface ServiceEntry {
  name: string;
  label: string;
  description?: string;
  url: string;
  new_window?: boolean;
}

// 单入口合并后 sidebar 顶部随板块切换：name 是顶部短名（w-40 侧栏放得下），
// title 是页面标题（沿用原 board-* 独立服务时代的 index.html 标题）
const sectionNames: Record<string, { name: string; title: string; icon: LucideIcon }> = {
  ops: { name: "运维", title: "运维中心", icon: Server },
  product: { name: "产品", title: "产品与任务看板", icon: Box },
  content: { name: "内容", title: "内容生产", icon: FileText },
  "email": { name: "邮件", title: "邮件", icon: Mail },
};


// urls.json 中 host 使用 xxxx 占位符，打开时替换为当前主机名
function resolveServiceUrl(raw: string): string {
  try {
    const u = new URL(raw);
    if (u.hostname === "xxxx") u.hostname = window.location.hostname;
    return u.toString();
  } catch {
    return raw;
  }
}

export interface NavItem {
  path: string;
  label: string;
  icon: LucideIcon;
}

// items 由各服务传入自己的导航子集（board 拆分为多独立服务后，
// 每个服务只渲染自己功能域的页面；顶部"切换服务"下拉仍列全部服务）。
// showUsers："用户管理"入口仅在运维中心（board-ops）展示（且需 admin），其他服务不传。
export function Sidebar({ items, showUsers }: { items: NavItem[]; showUsers?: boolean }) {
  const location = useLocation();
  const [services, setServices] = useState<ServiceEntry[]>([]);
  const [open, setOpen] = useState(false);
  const [currentUrl, setCurrentUrl] = useState<string | null>(null);
  const switcherRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    fetch("/urls.json")
      .then((r) => {
        if (!r.ok) throw new Error(`urls.json: ${r.status}`);
        return r.json();
      })
      .then((data: { services?: ServiceEntry[] }) => {
        const list = data.services || [];
        setServices(list);
        const resolved = list.map((s) => ({ ...s, url: resolveServiceUrl(s.url) }));
        // 匹配当前服务：host 相同 + 路径前缀相同。单容器合并后 /ops /product
        // /content /email 共用 host:port（:5181），只比 host 会永远命中
        // 第一项（运维中心）→ 被标为「当前」并 preventDefault 掉点击，导致切不回。
        // 取「路径前缀与当前 pathname 匹配且最长」的服务（/ops/ 是 /product/ 的
        // 前缀，故按前缀长度取最具体者）。外部服务（:8000/:8002…）host 不同，
        // path "/" 作为兵底前缀命中。
        const curPath = window.location.pathname;
        let best: { url: string; len: number } | null = null;
        for (const s of resolved) {
          try {
            const u = new URL(s.url);
            if (u.host !== window.location.host) continue;
            const prefix = u.pathname.replace(/\/+$/, "") || "/";
            const matched =
              curPath === prefix || curPath.startsWith(prefix + "/") || prefix === "/";
            if (!matched) continue;
            const len = prefix === "/" ? 0 : prefix.length;
            if (!best || len > best.len) best = { url: s.url, len };
          } catch {
            /* ignore */
          }
        }
        setCurrentUrl(best ? best.url : null);
      })
      .catch(() => setServices([]));
  }, []);

  // 点击下拉外部时关闭
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (switcherRef.current && !switcherRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const { isConnected, user } = useAuth();
  const isAdmin = !!isConnected && user?.roles.includes("admin");

  // 用户管理仅运维中心（board-ops，showUsers）且 admin 可见
  const navItems = [...items, ...(showUsers && isAdmin ? [{ path: "/users", label: "用户管理", icon: UserIcon }] : [])];

  // 单入口 SPA：当前路径首段即当前板块前缀（/ops /product /content /email）；
  // item.path 是板块相对路径（"/"、"/skills"...），渲染时拼上当前板块前缀，
  // 活跃态同样比较（原来靠各服务 BrowserRouter 的 basename 翻译）
  const prefix = location.pathname.split("/")[1] || "ops";
  const toSection = (p: string) => (p === "/" ? `/${prefix}/` : `/${prefix}${p}`);
  const section = sectionNames[prefix] || { name: "workflow", title: "workflow", icon: Workflow };
  const SectionIcon = section.icon;

  // 板块切换时同步页面标题（原各 board-* 独立服务的 index.html 标题）
  useEffect(() => {
    document.title = `${section.title} · workflow`;
  }, [section.title]);

  return (
    <aside className="w-40 shrink-0 flex flex-col bg-(--bg-sidebar)">
      <div ref={switcherRef} className="relative">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          title="切换服务"
          className="w-full py-4 pl-5 pr-3 border-b border-(--border-default) flex items-center gap-2 hover:bg-(--bg-elevated) transition-colors cursor-pointer"
        >
          <SectionIcon className="h-6 w-6 shrink-0 text-white" />
          <span className="text-lg text-white tracking-tight flex-1 text-left" title={section.title}>
            {section.name}
          </span>
          <ArrowLeftRight className="h-4 w-4 shrink-0 text-(--text-secondary)" />
        </button>
        {open && (
          <div className="absolute top-full left-0 w-52 z-20 border border-(--border-default) bg-(--bg-sidebar) shadow-2xl">
            {services.length === 0 ? (
              <div className="px-4 py-3 text-xs text-(--text-muted)">服务列表不可用</div>
            ) : (
              services.map((s) => {
                const url = resolveServiceUrl(s.url);
                const isCurrent = url === currentUrl;
                return (
                  <a
                    key={s.name || url}
                    href={url}
                    target={s.new_window ? "_blank" : undefined}
                    rel={s.new_window ? "noreferrer" : undefined}
                    onClick={(e) => {
                      // 普通跳转；目标服务未登录时会自动跳枢纽登录并带回（sso.ts SsoGate）
                      if (isCurrent) e.preventDefault();
                      setOpen(false);
                    }}
                    className={`group flex items-center gap-3 px-4 py-3 transition-colors ${isCurrent ? "bg-(--bg-elevated)" : "hover:bg-(--bg-elevated)"}`}
                  >
                    <span
                      className={`flex-1 text-sm font-medium truncate transition-colors ${isCurrent ? "text-white" : "text-(--text-secondary) group-hover:text-white"}`}
                    >
                      {s.label}
                    </span>
                    {isCurrent && <Check className="h-3.5 w-3.5 shrink-0 text-white" />}
                  </a>
                );
              })
            )}
          </div>
        )}
      </div>
      <nav className="flex-1 p-2 space-y-1">
        {navItems.map((item) => {
          // const Icon = item.icon;
          const to = toSection(item.path);
          // /ops（无尾斜杠）与 /ops/ 是同一页，根项需额外兼容
          const isActive = location.pathname === to || (item.path === "/" && location.pathname === `/${prefix}`);
          const cls = "w-full flex items-center gap-3 px-4 py-2.5 font-medium text-sm rounded-none transition-colors "
            + (isActive ? "bg-(--bg-elevated) text-white" : "text-(--text-secondary) hover:bg-(--bg-elevated) hover:text-white");
          return (
            <Link key={item.path} to={to} className={cls}>
              {item.label}
            </Link>
          );
        })}
      </nav>
      <UserBadge />
    </aside>
  );
}

// 登录状态栏：未登录 → “登录”按钮（跳枢纽 /login 页，登录框只在那里弹）；已登录 → 用户 + 登出
function UserBadge() {
  const { isConnected, user, logout } = useAuth();
  if (!isConnected) {
    return (
      <div className="p-3 border-t border-(--border-default)">
        <button
          type="button"
          onClick={() => redirectToLogin()}
          title="去登录页登录"
          className="w-full flex items-center justify-center gap-2 px-3 py-2 text-sm font-medium bg-white text-black rounded-none hover:opacity-90 transition-opacity cursor-pointer"
        >
          <LogIn className="h-4 w-4" /> 登录
        </button>
      </div>
    );
  }
  const label = user?.name || user?.email || user?.id || "用户";
  return (
    <div className="p-3 border-t border-(--border-default) flex items-center gap-2">
      <UserIcon className="h-4 w-4 text-(--text-secondary) shrink-0" />
      <span className="flex-1 text-xs text-(--text-secondary) truncate" title={label}>{label}</span>
      <button
        type="button"
        onClick={() => void logout()}
        title="登出"
        className="text-(--text-muted) hover:text-white transition-colors cursor-pointer"
      >
        <LogOut className="h-4 w-4" />
      </button>
    </div>
  );
}
