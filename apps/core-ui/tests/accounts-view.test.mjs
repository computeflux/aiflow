// M1-4 账号管理页读侧纯逻辑单测（node --test，无构建依赖）。
//
// 与 apps/social/internal/loginreport 的单测同口径（前端镜像实现）：
//   - 徽标：cooldown 未到期 = 冷却，到期自动回落活跃；未知状态按「需登录」展示；
//   - 今日配额：跨天（last_used_at 不是今天，UTC 日界）按 0 计；daily_limit<=0 = 不限；
//   - 最近活动 = max(last_used_at, last_login_at)；
//   - 渠道清单：注册表 + 账号表并集、去重、字典序。
import { strict as assert } from "node:assert";
import test from "node:test";

import {
    accountBadge,
    lastActiveAt,
    listChannels,
    quotaExhausted,
    quotaLabel,
    usedToday,
} from "../src/content/accounts.ts";

const now = new Date("2026-12-01T12:00:00Z");
const day = (iso) => new Date(iso);

const acc = (over = {}) => ({
    id: 1, channel: "xhs", account_id: "xhs-1", machine_id: "", owner: "", login_name: "",
    cookies_path: "", status: "active", used_today: 0, daily_limit: 0, last_error: "",
    last_login_error: "", ...over,
});

test("徽标：cooldown 未到期显示冷却，到期/无截止回落活跃", () => {
    assert.equal(accountBadge(acc({ status: "cooldown", cooldown_until: "2026-12-01T12:10:00Z" }), now), "cooldown");
    assert.equal(accountBadge(acc({ status: "cooldown", cooldown_until: "2026-12-01T11:59:59Z" }), now), "active");
    assert.equal(accountBadge(acc({ status: "cooldown" }), now), "active");
    assert.equal(accountBadge(acc({ status: "banned" }), now), "banned");
    assert.equal(accountBadge(acc({ status: "need_login" }), now), "need_login");
    assert.equal(accountBadge(acc({ status: "active" }), now), "active");
    // 未知状态（老数据/手工写入）→ need_login（一定不可调度，比假装活跃安全）。
    assert.equal(accountBadge(acc({ status: "" }), now), "need_login");
    assert.equal(accountBadge(acc({ status: "disabled" }), now), "need_login");
});

test("今日配额：跨天按 0 计（UTC 日界），不限额显示 x/-", () => {
    assert.equal(usedToday(acc({ used_today: 3, last_used_at: "2026-12-01T00:00:01Z" }), now), 3);
    assert.equal(usedToday(acc({ used_today: 3, last_used_at: "2026-11-30T23:59:59Z" }), now), 0);
    assert.equal(usedToday(acc({ used_today: 9 }), now), 0); // 从未用过

    assert.equal(quotaLabel(acc({ daily_limit: 50, used_today: 3, last_used_at: "2026-12-01T01:00:00Z" }), now), "3/50");
    assert.equal(quotaLabel(acc({ daily_limit: 50, used_today: 50, last_used_at: "2026-11-30T01:00:00Z" }), now), "0/50");
    // 不限额（daily_limit<=0）也要走跨天口径：昨日用 7 次、今日未用 → 今日 0。
    assert.equal(quotaLabel(acc({ daily_limit: 0, used_today: 7, last_used_at: "2026-12-01T01:00:00Z" }), now), "7/-");
    assert.equal(quotaLabel(acc({ daily_limit: 0, used_today: 7, last_used_at: "2026-11-30T01:00:00Z" }), now), "0/-");

    assert.equal(quotaExhausted(acc({ daily_limit: 5, used_today: 5, last_used_at: "2026-12-01T01:00:00Z" }), now), true);
    assert.equal(quotaExhausted(acc({ daily_limit: 5, used_today: 5, last_used_at: "2026-11-30T01:00:00Z" }), now), false);
    assert.equal(quotaExhausted(acc({ daily_limit: 0, used_today: 999 }), now), false);
});

test("最近活动 = max(使用, 登录)，皆空为 null", () => {
    assert.equal(lastActiveAt(acc({})), null);
    assert.deepEqual(lastActiveAt(acc({ last_used_at: "2026-11-30T10:00:00Z" })), day("2026-11-30T10:00:00Z"));
    assert.deepEqual(
        lastActiveAt(acc({ last_used_at: "2026-11-30T10:00:00Z", last_login_at: "2026-12-01T09:00:00Z" })),
        day("2026-12-01T09:00:00Z"),
    );
    assert.deepEqual(
        lastActiveAt(acc({ last_used_at: "2026-12-01T11:00:00Z", last_login_at: "2026-11-01T09:00:00Z" })),
        day("2026-12-01T11:00:00Z"),
    );
});

test("渠道清单：注册表与账号表并集、去重、字典序", () => {
    assert.deepEqual(
        listChannels(["xhs", "x", "xhs"], [acc({ channel: "bilibili" }), acc({ channel: "ali" }), acc({ channel: "" })]),
        ["ali", "bilibili", "x", "xhs"],
    );
    assert.deepEqual(listChannels([], []), []);
});
