import { Database, Layers, Users, Activity } from "lucide-react";
import { type SystemStatus } from "../api";

interface StatsHeaderProps {
  statusData: SystemStatus | null;
}

export function StatsHeader({ statusData }: StatsHeaderProps) {
  if (!statusData) return null;

  const stats = [
    { title: "素材总数", value: statusData.database?.total_raw_items ?? "-", icon: Database },
    { title: "抓取任务", value: statusData.database?.total_jobs ?? "-", icon: Layers },
    { title: "绑定账号", value: statusData.database?.total_accounts ?? "-", icon: Users },
    { title: "活跃工作流", value: (statusData.active_tasks ?? []).length, icon: Activity },
  ];

  return (
    <div className="mb-8 grid grid-cols-1 gap-4 sm:grid-cols-4">
      {stats.map((stat, i) => {
        const Icon = stat.icon;
        return (
          <div key={i} className="border border-(--border-subtle) p-4">
            <div className="flex justify-between items-center mb-2">
              <span className="text-[10px] font-bold uppercase tracking-widest text-(--text-muted)">
                {stat.title}
              </span>
              <Icon className="h-4 w-4 text-(--text-muted)" />
            </div>
            <p className="text-2xl font-black">{stat.value}</p>
          </div>
        );
      })}
    </div>
  );
}
