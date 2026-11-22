// 证书状态相关的共享常量、格式化工具与展示组件(Overview / Status 页面共用)。
import type { ReactNode } from "react";
import type { CertReport, CertStatus } from "../api";

export const statusMeta: Record<CertStatus, { label: string; tone: string }> = {
  OK: { label: "OK", tone: "text-(--success)" },
  WARN: { label: "WARN", tone: "text-(--warning)" },
  EXPIRED: { label: "EXPIRED", tone: "text-(--danger)" },
  ERROR: { label: "ERROR", tone: "text-(--danger)" },
};

export const statusDot: Record<CertStatus, string> = {
  OK: "bg-(--success)",
  WARN: "bg-(--warning)",
  EXPIRED: "bg-(--danger)",
  ERROR: "bg-(--danger)",
};

export function formatTime(iso?: string): string {
  if (!iso) return "-";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("zh-CN", { hour12: false });
}

export function fmtDays(r: CertReport): string {
  if (r.status === "ERROR") return "n/a";
  if (r.days_left < 0) return `已过期 ${Math.abs(r.days_left).toFixed(1)} 天`;
  return `${r.days_left.toFixed(1)} 天`;
}

export function Badge({ status }: { status: CertStatus }) {
  const meta = statusMeta[status];
  return (
    <span className={`inline-flex items-center gap-1.5 text-xs font-bold uppercase ${meta.tone}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${statusDot[status]}`} />
      {meta.label}
    </span>
  );
}

export function StatCard({
  icon,
  label,
  value,
  tone,
}: {
  icon: ReactNode;
  label: string;
  value: string;
  tone: string;
}) {
  return (
    <div className="border border-(--border-subtle) bg-(--bg-elevated) p-4">
      <div className="flex items-center gap-2 text-(--text-muted)">
        {icon}
        <span className="text-xs font-bold uppercase">{label}</span>
      </div>
      <div className={`mt-3 text-3xl font-bold tracking-tighter ${tone}`}>{value}</div>
    </div>
  );
}
