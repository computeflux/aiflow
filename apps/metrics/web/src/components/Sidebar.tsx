import { Link, useLocation } from "react-router-dom";
import { useEffect, useRef, useState } from "react";
import {
  LayoutDashboard,
  ChevronDown,
  Workflow,
  Terminal,
  Radar,
  Bot,
  Check,
  Globe,
  ShieldCheck,
  Globe2,
  Activity,
  Gauge,
  Settings,
  type LucideIcon,
} from "lucide-react";

interface ServiceEntry {
  name: string;
  label: string;
  description?: string;
  url: string;
  new_window?: boolean;
}

// 按 urls.json 中的 name 匹配图标
const serviceIcons: Record<string, LucideIcon> = {
  "metrics-monitor": Gauge,
  "tee-monitor": Activity,
  workflow: Workflow,
  "contract-console": Terminal,
  "chain-monitor": Radar,
  coder: Bot,
};

// urls.json 由 workflow board 服务集中管理: 用当前 URL 的 host 拼接
// http://<当前主机名>:5181/urls.json（board-ops = 枢纽，多 board 拆分之首）。
// 也可通过 VITE_URLS_JSON 环境变量覆盖地址。
const urlsJsonUrl = (() => {
  const raw = import.meta.env.VITE_URLS_JSON as string | undefined;
  if (raw && raw.trim()) return raw.trim();
  return `http://${window.location.hostname}:5181/urls.json`;
})();

// urls.json 中服务条目的 host 使用 xxxx 占位符，打开时替换为当前主机名
function resolveServiceUrl(raw: string): string {
  try {
    const u = new URL(raw);
    if (u.hostname === "xxxx") u.hostname = window.location.hostname;
    return u.toString();
  } catch {
    return raw;
  }
}

export function Sidebar() {
  const location = useLocation();
  const [services, setServices] = useState<ServiceEntry[]>([]);
  const [open, setOpen] = useState(false);
  const [currentUrl, setCurrentUrl] = useState<string | null>(null);
  const switcherRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    fetch(urlsJsonUrl)
      .then((r) => {
        if (!r.ok) throw new Error(`urls.json: ${r.status}`);
        return r.json();
      })
      .then((data: { services?: ServiceEntry[] }) => {
        const list = data.services || [];
        setServices(list);
        const resolved = list.map((s) => ({ ...s, url: resolveServiceUrl(s.url) }));
        const cur = resolved.find((s) => {
          try {
            return new URL(s.url).host === window.location.host;
          } catch {
            return false;
          }
        });
        setCurrentUrl(cur ? cur.url : null);
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

  const navItems = [
    { path: "/", label: "总览", icon: LayoutDashboard },
    { path: "/certs", label: "证书状态", icon: ShieldCheck },
    { path: "/hosts", label: "域名管理", icon: Globe2 },
    { path: "/config", label: "设置", icon: Settings },
  ];

  return (
    <aside className="w-40 flex flex-col bg-(--bg-sidebar)">
      <div ref={switcherRef} className="relative">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          title="切换服务"
          className="w-full py-4 pl-5 pr-3 border-b border-(--border-default) flex items-center gap-2 hover:bg-(--bg-elevated) transition-colors cursor-pointer"
        >
          <Gauge className="h-6 w-6 text-white shrink-0" />
          <span className="text-lg text-white tracking-tight flex-1 text-left">metrics</span>
          <ChevronDown
            className={`h-3.5 w-3.5 text-(--text-secondary) transition-transform ${open ? "rotate-180" : ""}`}
          />
        </button>
        {open && (
          <div className="absolute top-full left-0 w-52 z-20 border border-(--border-default) bg-(--bg-sidebar) shadow-2xl">
            <div className="px-4 pt-3 pb-2 text-[10px] font-bold uppercase tracking-widest text-(--text-muted) border-b border-(--border-divider)">
              切换服务
            </div>
            {services.length === 0 ? (
              <div className="px-4 py-3 text-xs text-(--text-muted)">服务列表不可用</div>
            ) : (
              services.map((s) => {
                const url = resolveServiceUrl(s.url);
                const isCurrent = url === currentUrl;
                const Icon = serviceIcons[s.name] || Globe;
                return (
                  <a
                    key={s.name || url}
                    href={url}
                    target={s.new_window ? "_blank" : undefined}
                    rel={s.new_window ? "noreferrer" : undefined}
                    onClick={(e) => {
                      if (isCurrent) e.preventDefault();
                      setOpen(false);
                    }}
                    className={`group flex items-center gap-3 px-4 py-3 transition-colors ${
                      isCurrent ? "bg-(--bg-elevated)" : "hover:bg-(--bg-elevated)"
                    }`}
                  >
                    <span
                      className={`shrink-0 transition-colors ${
                        isCurrent ? "text-white" : "text-(--text-secondary) group-hover:text-white"
                      }`}
                    >
                      <Icon className="h-4 w-4" />
                    </span>
                    <span
                      className={`flex-1 text-sm font-medium truncate transition-colors ${
                        isCurrent ? "text-white" : "text-(--text-secondary) group-hover:text-white"
                      }`}
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
          const isActive = location.pathname === item.path;
          return (
            <Link
              key={item.path}
              to={item.path}
              className={`w-full flex items-center gap-3 px-4 py-2.5 font-medium text-sm rounded-none transition-colors ${
                isActive
                  ? "bg-(--bg-elevated) text-white"
                  : "text-(--text-secondary) hover:bg-(--bg-elevated) hover:text-white"
              }`}
            >
              {(() => {
                const Icon = item.icon;
                return <Icon className="h-4 w-4 shrink-0" />;
              })()}
              {item.label}
            </Link>
          );
        })}
      </nav>
      <div className="p-4 text-[10px] text-(--text-disabled) font-mono">
        PROJECT
        <br />
        MONITOR v1
      </div>
    </aside>
  );
}