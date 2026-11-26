// M1-4 账号管理页（core-ui /content/accounts）的「渠道过滤 / 状态徽标 / 今日配额 /
// 最近活动」读侧纯逻辑（apps/social internal/loginreport 的前端镜像，无 React 依赖）。
//
// 为什么要有这一层：徽标不是 status 列的原文回显 ——
//   - status=cooldown 且 cooldown_until 已过 → 实际显示 active（冷却到期自动回落，
//     与 store.IsAccountUsable 同语义）；
//   - 配额要显示 used_today/daily_limit，且跨天（last_used_at 不是今天）按 0 计
//     （与 store.IsAccountQuotaFree 同语义：达到限额那天落 need_login 的账号，
//     次日运维置回 active 后计数不能继续算昨天的）。
// 这些判定散在页面里写会漂，收成一个可单测的纯函数
// （单测：apps/core-ui/tests/accounts-view.test.mjs，`npm test` = node --test 直跑
// 本 .ts 源，零构建步骤 —— core-ui 没有测试运行器/打包期转译，见 package.json scripts）。
//
// SocialAccountView 在这里做**结构声明**而不是 import kit：视图层只读这些字段，与
// src/kit/api.ts 的同名接口逐字段一致，页面使用处把 kit 的接口传进来天然兼容；
// 不 import 是为了让本文件能被 node --test 直跑（.ts 里任何跨文件 import 都会让
// 零构建的测试跑不起来）。
export interface SocialAccountView {
    id: number;
    channel: string;
    account_id: string;
    machine_id: string;
    owner: string;
    login_name: string;
    cookies_path: string;
    status: string; // active | need_login | banned | cooldown
    cooldown_until?: string;
    last_used_at?: string;
    used_today: number;
    daily_limit: number;
    last_error: string;
    last_login_at?: string;
    last_login_error: string;
}

export type AccountBadge = "active" | "need_login" | "banned" | "cooldown";

export const ACCOUNT_BADGES: AccountBadge[] = ["active", "need_login", "banned", "cooldown"];

export const BADGE_LABELS: Record<AccountBadge, string> = {
    active: "活跃",
    need_login: "需登录",
    banned: "封号",
    cooldown: "冷却",
};

/** 状态徽标：status=cooldown 且冷却未到期才显示「冷却」，到期自动回落「活跃」。 */
export function accountBadge(a: SocialAccountView, now: Date = new Date()): AccountBadge {
    if (a.status === "cooldown") {
        const until = a.cooldown_until ? new Date(a.cooldown_until).getTime() : NaN;
        if (!Number.isNaN(until) && now.getTime() < until) return "cooldown";
        return "active";
    }
    if ((ACCOUNT_BADGES as string[]).includes(a.status)) return a.status as AccountBadge;
    // 库里出现未知状态（老数据/手工写入）时按「需登录」展示：它一定不可调度，
    // 比按「活跃」展示更安全（不会让人以为账号在用）。
    return "need_login";
}

/** 今日已用次数（跨天按 0 计，UTC 日界，与后端 MarkAccountUsed 的 CURDATE 同日界）。 */
export function usedToday(a: SocialAccountView, now: Date = new Date()): number {
    const last = a.last_used_at ? new Date(a.last_used_at).getTime() : NaN;
    if (Number.isNaN(last)) return 0;
    return dayStartUTC(new Date(last)).getTime() === dayStartUTC(now).getTime() ? a.used_today : 0;
}

/** 今日配额用量文本："3/50"；daily_limit <= 0 = 不限 → "3/-"。 */
export function quotaLabel(a: SocialAccountView, now: Date = new Date()): string {
    const used = usedToday(a, now);
    return a.daily_limit > 0 ? `${used}/${a.daily_limit}` : `${used}/-`;
}

/** 今日配额是否已用尽（限额 > 0 且今日计数达到限额）。 */
export function quotaExhausted(a: SocialAccountView, now: Date = new Date()): boolean {
    return a.daily_limit > 0 && usedToday(a, now) >= a.daily_limit;
}

/** 最近活动时间 = max(last_used_at, last_login_at)；两者皆空 = null（列表显示 "-"）。 */
export function lastActiveAt(a: SocialAccountView): Date | null {
    const used = a.last_used_at ? new Date(a.last_used_at) : null;
    const login = a.last_login_at ? new Date(a.last_login_at) : null;
    if (!used) return login && !Number.isNaN(login.getTime()) ? login : null;
    if (Number.isNaN(used.getTime())) return login && !Number.isNaN(login.getTime()) ? login : null;
    if (!login || Number.isNaN(login.getTime())) return used;
    return login.getTime() > used.getTime() ? login : used;
}

/** 渠道 tab 的渠道清单（注册表优先、账号表兜底，去重 + 字典序）。 */
export function listChannels(channels: string[], accounts: SocialAccountView[]): string[] {
    const set = new Set<string>(channels.filter(Boolean));
    for (const a of accounts) if (a.channel) set.add(a.channel);
    return [...set].sort();
}

function dayStartUTC(d: Date): Date {
    return new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()));
}
