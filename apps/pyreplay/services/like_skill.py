"""like 技能内核（M3-2：like 能力兼容 pyreplay skill schema）。

本模块是 skill 'like' 的**唯一实现**：把「对一条推文用最多 N 个账号点赞」收敛成
一个可注入依赖的纯逻辑函数 :func:`LikeSkill.run`，由三条入口共用同一内核，
保证行为与 ``skill.json``（skill schema）逐字段一致：

* ``skill_server.py``  —— HTTP ``POST /invoke``（skillgateway 调用，输入输出即 schema）；
* ``main.py``          —— Temporal activity ``LikeActivities.like`` / ``like_batch``；
* 后台 like_jobs 执行器 —— 会话式批量点赞（见 main.py._execute_like_job）。

schema 契约（与 apps/pyreplay/skill.json 完全对应，字段名/类型/上限一字不差）：

    input  = {"post_url": "https://x.com/.../status/<id>", "like_count": 1..20}
    output = {"ok": bool, "reason": str, "tweet_id": str, "attempts": int,
              "success_count": int, "results": [{"tweet_id", "success", "error"}]}

关键约定（旧实现散落在 skill_server / main.py 各处，本次收口到这里）：

* **账号级去重**：同一次调用内每个账号只用一次；跨调用由调用方把已点过的账号
  id 通过 ``excluded_account_ids`` 传进来（like_jobs 执行器的口径）。
* **账号记账**：取账号用 ``accounts.pick_account(excluded)``、用完立即
  ``accounts.mark_used(account.id)``。两个动作的**策略在实现侧**：本模块只按接缝调用，
  冷却/每日限额/「按会话计一次」由注入的账号池自己决定（worker 与 skill HTTP 进程逐条
  记账；like_jobs 会话执行器把 ``mark_used`` 做成 no-op，整场会话结束时统一记一次，
  见 main.py 的 ``_SessionAccountPool``）。本模块不复制账号池那份 SQL。
* **点赞执行**：``liker(target, cookies)`` 是可注入接缝（生产 = XLikeAdapter.like，
  单测 = 假适配器），异常不外抛：一次尝试崩掉记成一条失败结果继续下一轮。
* **落库**：逐条结果写 ``like_records``（``store.save_like_record``），失败只打日志；
  全部尝试失败时写 ``error_logs``（``store.log_error``），board 的错误日志页可见。
* **绝不冒泡**：落库/记账的异常一律吞成日志，技能永不因副作用失败而抛栈。

设计上是 async 的（浏览器适配器是协程）；同步的 HTTP 线程入口在 skill_server.py
里用 ``asyncio.run`` 收敛。本模块**不在顶层 import pymysql / crawl4ai / adapters**：
账号池、点赞执行器、落库全部由调用方注入（鸭子类型，见下面的 Protocol），连
``adapters.models`` 的两个数据模型都是函数内延迟 import —— 因此在没有装任何第三方
依赖的环境里也能导入并跑通单测（CI 的无 .venv 兜底口径依赖这一点）。
"""

from __future__ import annotations

import asyncio
import json
import logging
import re
from collections.abc import Awaitable, Callable
from typing import TYPE_CHECKING, Any, Protocol

if TYPE_CHECKING:  # 仅类型标注；运行时不导入 adapters（会把 crawl4ai 整条依赖链拖进来）
    from adapters.models import LikeResult, LikeTarget

logger = logging.getLogger("skill.like.core")

# skill schema 的取值口径（与 skill.json 的 properties.like_count 一致）：
# like_count 缺省 1，上限 20（同一推文最多用 20 个不同账号各点一次）。
DEFAULT_LIKE_COUNT = 1
MAX_LIKE_COUNT = 20

_STATUS_RE = re.compile(r"/status/(\d+)")


def _cookies_from_tokens(auth_token: str, ct0: str) -> list[dict[str, Any]]:
    """按 X 的 cookie 规格拼装 auth_token / ct0（缺失的键不写空值）。

    ⚠️ 这是 :func:`adapters.x_like.cookies_from_tokens` 的**第二份实现**（字段名、domain、
    secure/httpOnly 逐项一致）：技能内核必须在没有浏览器依赖的环境里可导入（单测、无
    .venv 的 CI 兜底），所以不能引那条 import 链。**改任何一侧的 cookie 规格必须同步另一侧**，
    漂移的后果是点赞带着「未登录」的会话跑，只在 like_records 里留下一片失败。
    """
    cookies: list[dict[str, Any]] = []
    if auth_token:
        cookies.append({
            "name": "auth_token",
            "value": auth_token,
            "domain": ".x.com",
            "path": "/",
            "secure": True,
            "httpOnly": True,
        })
    if ct0:
        cookies.append({
            "name": "ct0",
            "value": ct0,
            "domain": ".x.com",
            "path": "/",
            "secure": True,
            "httpOnly": False,
        })
    return cookies


def account_cookies(account: Any) -> list[dict[str, Any]]:
    """账号 → Playwright cookies（auth_token / ct0，域名 .x.com）；无账号 = 空列表。"""
    if account is None:
        return []
    return _cookies_from_tokens(
        getattr(account, "auth_token", "") or "", getattr(account, "ct0", "") or ""
    )



def _like_target(tweet_id: str, post_url: str, author: str = "", content: str = "") -> Any:
    """构造 adapters.models.LikeTarget（延迟 import：见模块 docstring 的依赖约定）。"""
    from adapters.models import LikeTarget

    return LikeTarget(tweet_id=tweet_id, tweet_url=post_url, author=author, content=content)


def _failed_result(target: Any, error: str) -> Any:
    """构造一条失败的 LikeResult（点赞尝试崩掉时的兜底记录）。"""
    from adapters.models import LikeResult

    return LikeResult(
        tweet_id=target.tweet_id,
        tweet_url=target.tweet_url,
        success=False,
        error=error,
    )


def extract_tweet_id(post_url: str) -> str:
    """从推文 URL 里取纯数字 tweet id（``.../status/<id>``）；取不到返回空串。"""
    m = _STATUS_RE.search(post_url or "")
    return m.group(1) if m else ""


def clamp_like_count(raw: Any) -> int:
    """把输入里的 like_count 规整到 [1, MAX_LIKE_COUNT]。

    None / 非法值 / <1 → 1（schema 的默认值）；>20 → 20（schema 的 maximum）。
    skillgateway 会先用 input_schema 拦掉越界输入，这里的钳制是给直连 HTTP
    （``POST /invoke``）与 Temporal activity（Go 侧直接传参）两条内部链路兜底。
    """
    if raw is None:
        return DEFAULT_LIKE_COUNT
    try:
        count = int(raw)
    except (TypeError, ValueError):
        return DEFAULT_LIKE_COUNT
    if count < 1:
        return DEFAULT_LIKE_COUNT
    return min(count, MAX_LIKE_COUNT)


class AccountPicker(Protocol):
    """账号池接缝（实现：services.account_manager.AccountManager）。"""

    def pick_account(self, excluded: set[int] | None = None) -> Any: ...

    def mark_used(self, account_id: int) -> None: ...


class LikeExecutor(Protocol):
    """点赞执行接缝（实现：adapters.x_like.XLikeAdapter.like）。"""

    async def like(
        self, target: LikeTarget, cookies: list[dict[str, Any]] | None = None
    ) -> LikeResult: ...


class LikeRecordStore(Protocol):
    """落库接缝（实现：skill_server / main.py 里的 MySQL 写入闭包）。"""

    def save_like_record(
        self, result: LikeResult, account: Any = None
    ) -> None: ...

    def log_error(self, source: str, message: str, context: str = "") -> None: ...


class LikeSkill:
    """skill 'like' 的内核：一条推文 × 最多 N 个账号点赞。

    Args:
        accounts: 账号池（None = 不取账号，全部尝试走 liker 的缺省登录态）。
        liker: 点赞执行器（必填）。
        store: 落库接缝（None = 不落库，只回报结果）。
        machine_id: 本机标识，随点赞记录落库（与 pyreplay worker 同一口径）。
        delay_between: 同一推文多次点赞之间的随机抖动秒数（反封号；0 = 不等）。
        sleep: 等待接缝（测试注入，避免真等）。
    """

    def __init__(
        self,
        liker: LikeExecutor,
        accounts: AccountPicker | None = None,
        store: LikeRecordStore | None = None,
        machine_id: str = "",
        delay_between: float = 0.0,
        sleep: Callable[..., Awaitable[None]] = asyncio.sleep,
    ) -> None:
        self.liker = liker
        self.accounts = accounts
        self.store = store
        self.machine_id = (machine_id or "")[:64]
        self.delay_between = delay_between
        self._sleep = sleep

    async def run(
        self,
        post_url: str,
        like_count: Any = DEFAULT_LIKE_COUNT,
        *,
        content: str = "",
        author: str = "",
        excluded_account_ids: set[int] | None = None,
    ) -> dict[str, Any]:
        """按 skill schema 点赞一条推文，返回 output_schema 的结构化结果。

        ``ok`` 的语义与 skillgateway 的判定一致：``success_count > 0``。
        ``attempts`` 是实际发起的点赞尝试数（= 规整后的 like_count）。
        """
        count = clamp_like_count(like_count)
        tweet_id = extract_tweet_id(post_url)
        if not tweet_id:
            # 输入侧硬失败：schema 已要求 post_url，这里兜底「有 URL 但没有 id」。
            self._log_error(
                "skill.like",
                f"like rejected: cannot extract tweet id from post_url: {post_url!r}",
                json.dumps({"post_url": post_url, "machine_id": self.machine_id}, ensure_ascii=False),
            )
            return {
                "ok": False,
                "reason": f"cannot extract tweet id from post_url: {post_url!r}",
                "tweet_id": "",
                "attempts": 0,
                "success_count": 0,
                "results": [],
            }

        target = _like_target(tweet_id, post_url, author, content)

        results: list[dict[str, Any]] = []
        success = 0
        used_ids: set[int] = set(excluded_account_ids or set())

        for attempt in range(count):
            account = self._pick_account(used_ids)
            cookies = self._cookies(account)
            result = await self._like_once(target, cookies, attempt + 1)

            if account is not None:
                used_ids.add(account.id)
                self._mark_used(account.id)

            if result.success:
                success += 1
            self._save_record(result, account)
            results.append({
                "tweet_id": result.tweet_id,
                "success": bool(result.success),
                "error": result.error or "",
            })

            if attempt < count - 1 and self.delay_between > 0:
                await self._sleep(self.delay_between)

        reason = "" if success > 0 else self._failure_reason(results)
        if success == 0:
            self._log_error(  # noqa: E501
                "skill.like",
                f"like failed: tweet {tweet_id} attempts={count}: {reason}",
                json.dumps(
                    {
                        "tweet_id": tweet_id,
                        "post_url": post_url,
                        "like_count": count,
                        "machine_id": self.machine_id,
                        "results": results,
                    },
                    ensure_ascii=False,
                ),
            )

        return {
            "ok": success > 0,
            "reason": reason,
            "tweet_id": tweet_id,
            "attempts": count,
            "success_count": success,
            "results": results,
            # account_ids：本次调用实际用过的账号 id（含调用方传进来的 excluded）。
            # skill.json 的 output_schema 未列该字段，但 JSON Schema 默认允许附加属性，
            # skillgateway 的输出校验照旧通过。调用方据此把账号去重范围从「一次调用」
            # 扩到「一批调用」：批量入口与 like_jobs 会话执行器都靠它维持
            # 「同一账号不重复点同一条推文」的口径。
            "account_ids": sorted(used_ids),
        }

    async def run_batch(
        self,
        items: list[dict[str, Any]],
        *,
        like_count: Any = DEFAULT_LIKE_COUNT,
        delay_between: float | None = None,
        excluded_account_ids: set[int] | None = None,
    ) -> list[dict[str, Any]]:
        """按 schema 逐条点赞多条推文（条目形如 ``{"post_url", "like_count"}``）。

        给「一个进程内连续点多条」的入口用：条目内 ``like_count`` 优先于全局参数，
        条目之间按 ``delay_between`` 抖动。单条的失败绝不冒泡（run 内部已消化），
        批量层只负责顺序与节奏。

        ⚠️ 当前三条生产入口都不走它：``LikeActivities.like_batch`` 与 like_jobs 会话执行器
        要在外层做自己的账号去重与会话记账，逐条调 :meth:`run`；本方法是内核侧的批量
        语义基准（单测覆盖），新增入口优先复用它，不要再手写一份循环。
        """
        out: list[dict[str, Any]] = []
        total = len(items)
        for i, item in enumerate(items):
            out.append(
                await self.run(
                    str(item.get("post_url") or ""),
                    item.get("like_count", like_count),
                    content=str(item.get("content") or ""),
                    author=str(item.get("author") or ""),
                    excluded_account_ids=set(excluded_account_ids or set()),
                )
            )
            if i < total - 1 and (delay_between or 0) > 0:
                await self._sleep(delay_between)
        return out

    # —— 接缝封装：任何一侧抛异常都不许打断点赞循环 ——

    def _pick_account(self, excluded: set[int]) -> Any:
        if self.accounts is None:
            return None
        try:
            return self.accounts.pick_account(excluded)
        except Exception as exc:  # noqa: BLE001
            logger.warning("pick_account failed: %s", exc)
            return None

    def _cookies(self, account: Any) -> list[dict[str, Any]] | None:
        """账号 → 登录态 cookies；无账号时返回 None（由 liker 决定兜底登录态）。"""
        if account is None:
            return None
        return account_cookies(account)

    async def _like_once(self, target: Any, cookies: Any, attempt: int) -> Any:
        try:
            return await self.liker.like(target, cookies=cookies)
        except Exception as exc:  # noqa: BLE001
            logger.warning("like attempt %d crashed: %s", attempt, exc)
            return _failed_result(target, f"like attempt crashed: {exc}")

    def _mark_used(self, account_id: int) -> None:
        if self.accounts is None:
            return
        try:
            self.accounts.mark_used(account_id)
        except Exception as exc:  # noqa: BLE001
            logger.warning("mark_used(%s) failed: %s", account_id, exc)

    def _save_record(self, result: LikeResult, account: Any) -> None:
        if self.store is None:
            return
        try:
            self.store.save_like_record(result, account)
        except Exception as exc:  # noqa: BLE001
            logger.warning("save like record failed: %s", exc)

    def _log_error(self, source: str, message: str, context: str = "") -> None:
        if self.store is None:
            return
        try:
            self.store.log_error(source, message, context)
        except Exception as exc:  # noqa: BLE001
            logger.warning("save error log failed: %s", exc)

    @staticmethod
    def _failure_reason(results: list[dict[str, Any]]) -> str:
        """全失败时的 reason：取首条尝试的错误原因，无则给统一话术。"""
        for r in results:
            if r.get("error"):
                return str(r["error"])
        return "all like attempts failed"
