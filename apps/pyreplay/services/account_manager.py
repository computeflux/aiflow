"""随机账号池：多账号轮换、冷却、每日限额。

反封号策略：
- 每次使用后记录 last_used_at，冷却期内不再选中
- 每日限额（used_today >= daily_like_limit）自动跳过
- 挑选时按"最久未用"排序，只从前 1/3 里随机选——
  既保证账号被分散使用，又避免严格的轮询规律
"""

from __future__ import annotations

import os
import random
from datetime import UTC, datetime, timedelta

from adapters.models import XAccount
from services.mysql import get_mysql

# 本机（机器人）标识：只操作属于自己的账号列表。
# 每台机器人部署时在 .env 里设置不同的 MACHINE_ID。
_MACHINE_ID = os.environ.get("MACHINE_ID", "")

_SELECT_SQL = (
    "SELECT id, username, auth_token, ct0, enabled, daily_like_limit, "
    "cooldown_minutes, used_today, last_used_at "
    "FROM x_accounts WHERE enabled = 1 AND machine_id = %s"
)


class AccountManager:
    """X 账号池管理器（基于 MySQL x_accounts 表）。"""

    def __init__(self) -> None:
        # 惰性连接：不在 __init__ 里连 MySQL，避免容器启动时 MySQL 未就绪
        # 导致 skill_server.py / main.py 进程在 import 阶段即崩溃。
        # get_mysql() 内部已有连接缓存 + ping 断线重连逻辑，每次调用安全。
        pass

    def load_eligible(self, exclude_id: int | None = None, ignore_daily_limit: bool = False) -> list[XAccount]:
        """加载可用账号：属于本机、启用、（可选）未达每日限额、冷却期已过。

        ignore_daily_limit=True 时忽略每日限额（订阅分发场景不再限量）。
        """
        conn = get_mysql()
        with conn.cursor() as cur:
            cur.execute(_SELECT_SQL, (_MACHINE_ID,))
            rows = cur.fetchall()

        now = datetime.now(UTC)
        accounts: list[XAccount] = []
        for r in rows:
            # 数据库列可空，防御 None（老数据/手工插入可能为 NULL）
            used_today = int(r["used_today"] or 0)
            daily_limit = int(r["daily_like_limit"] or 0)
            cooldown_min = int(r["cooldown_minutes"] or 0)
            if not ignore_daily_limit and used_today >= daily_limit:
                continue

            last = r["last_used_at"]
            if last is not None:
                if last.tzinfo is None:
                    last = last.replace(tzinfo=UTC)
                cooldown = timedelta(minutes=cooldown_min)
                if now - last < cooldown:
                    continue

            if exclude_id is not None and r["id"] == exclude_id:
                continue

            accounts.append(
                XAccount(
                    id=int(r["id"]),
                    username=r["username"] or "",
                    auth_token=r["auth_token"] or "",
                    ct0=r["ct0"] or "",
                    enabled=bool(r["enabled"]),
                    daily_like_limit=daily_limit,
                    cooldown_minutes=cooldown_min,
                    used_today=used_today,
                    last_used_at=last,
                )
            )
        return accounts

    def pick(self, exclude_id: int | None = None) -> XAccount | None:
        """随机挑选一个账号（避免与 exclude_id 相同，防止连续同一账号）。"""
        accounts = self.load_eligible(exclude_id)
        if not accounts:
            return None

        return self._pick_from(accounts)

    def pick_exclude_many(
        self, excluded: set[int] | None = None, ignore_daily_limit: bool = True
    ) -> XAccount | None:
        """随机挑选一个账号，排除已点赞过该推文的账号集合。

        订阅分发场景使用：同一推文每台机器需要所有账号各点一次，
        因此排除已点过的账号；默认忽略每日限额（不再限量）。
        """
        excluded = excluded or set()
        accounts = [
            a for a in self.load_eligible(ignore_daily_limit=ignore_daily_limit)
            if a.id not in excluded
        ]
        if not accounts:
            return None
        return self._pick_from(accounts)

    @staticmethod
    def _pick_from(accounts: list[XAccount]) -> XAccount:
        """最久未用的排前面，只从前 1/3 里随机挑：分散使用 + 随机性。"""
        accounts = sorted(
            accounts,
            key=lambda a: a.last_used_at or datetime.min.replace(tzinfo=UTC),
        )
        pool = accounts[: max(1, (len(accounts) + 2) // 3)]
        return random.choice(pool)

    def mark_used(self, account_id: int) -> None:
        """记录一次使用：跨天重置 used_today，否则 +1，并刷新 last_used_at。"""
        conn = get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "UPDATE x_accounts SET "
                "used_today = IF(DATE(last_used_at) < CURDATE(), 1, used_today + 1), "
                "last_used_at = NOW() "
                "WHERE id = %s",
                (account_id,),
            )
