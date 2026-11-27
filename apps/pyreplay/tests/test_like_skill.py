"""M3-2 单元测试：like 能力兼容 pyreplay skill schema（工作项 wi-1795707934-8748）。

钉住 skill 'like' 的契约（apps/pyreplay/skill.json）与账号池行为：

    1. 输入规整：like_count 缺省/非法/越界 → 钳到 [1, 20]；post_url 取不到 id → ok=false；
    2. 输出形状：与 output_schema 逐字段一致（ok/reason/tweet_id/attempts/success_count/results）；
    3. ok 的语义 = success_count > 0（skillgateway 依顶层 ok 判定 skill_runs 状态）；
    4. 账号级去重：同一次调用内每个账号只用一次，且用完立刻 mark_used（冷却/限额记账）；
    5. excluded_account_ids 生效：跨调用已点过的账号不再被选中；
    6. 点赞执行器抛异常 → 记成一条失败尝试，绝不冒泡打断剩余尝试；
    7. 逐条落库 like_records；全失败时写 error_logs（board 错误日志页可见）；
    8. 落库接缝抛异常 → 吞成日志，技能结果不受影响。

CI 安全：不联网、不起浏览器、不连 MySQL —— 账号池 / 点赞执行器 / 落库全部鸭子注入。

用法（在 pyreplay 目录下）：
    uv run python3 tests/test_like_skill.py          # 无 pytest 时的直跑契约
    uv run pytest tests/test_like_skill.py -q        # 有 pytest 时
"""

from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path
from typing import Any

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from adapters.models import LikeResult, LikeTarget  # noqa: E402
from services.like_skill import (  # noqa: E402
    DEFAULT_LIKE_COUNT,
    MAX_LIKE_COUNT,
    LikeSkill,
    clamp_like_count,
    extract_tweet_id,
)

POST_URL = "https://x.com/computefluxAI/status/2084675512367206779"
TWEET_ID = "2084675512367206779"


class Account:
    """x_accounts 行的鸭子替身（只带点赞链用到的字段）。"""

    def __init__(self, id: int, username: str = "") -> None:
        self.id = id
        self.username = username or f"bot{id}"
        self.auth_token = f"tok{id}"
        self.ct0 = f"ct0-{id}"


class FakeAccounts:
    """账号池鸭子替身：按序发放账号，记录取用与记账调用。"""

    def __init__(self, accounts: list[Account]) -> None:
        self.accounts = accounts
        self.picked_excluded: list[set[int]] = []
        self.marked: list[int] = []

    def pick_account(self, excluded: set[int] | None = None) -> Account | None:
        excluded = excluded or set()
        self.picked_excluded.append(set(excluded))
        for a in self.accounts:
            if a.id not in excluded:
                return a
        return None

    def mark_used(self, account_id: int) -> None:
        self.marked.append(account_id)


class FakeLikeResult(LikeResult):
    """记录本次调用收到的 cookies（鸭子断言用）。"""


class FakeLiker:
    """点赞执行器鸭子替身：按脚本逐次返回成功/失败/抛异常。"""

    def __init__(self, outcomes: list[Any]) -> None:
        self.outcomes = outcomes
        self.calls: list[tuple[str, Any]] = []

    async def like(
        self, target: LikeTarget, cookies: list[dict[str, Any]] | None = None
    ) -> LikeResult:
        self.calls.append((target.tweet_id, cookies))
        outcome = self.outcomes[min(len(self.calls) - 1, len(self.outcomes) - 1)]
        if isinstance(outcome, Exception):
            raise outcome
        return LikeResult(
            tweet_id=target.tweet_id,
            tweet_url=target.tweet_url,
            success=bool(outcome),
            error="" if outcome else "like button not found",
        )


class FakeStore:
    """落库接缝鸭子替身（like_records + error_logs）。"""

    def __init__(self, raise_on_save: bool = False) -> None:
        self.records: list[tuple[str, bool, Any]] = []
        self.errors: list[tuple[str, str, str]] = []
        self.raise_on_save = raise_on_save

    def save_like_record(self, result: LikeResult, account: Any = None) -> None:
        if self.raise_on_save:
            raise RuntimeError("mysql down")
        self.records.append((result.tweet_id, bool(result.success), account))

    def log_error(self, source: str, message: str, context: str = "") -> None:
        self.errors.append((source, message, context))


def _run(coro: Any) -> Any:
    return asyncio.run(coro)


def _skill(
    outcomes: list[Any],
    accounts: list[Account] | None = None,
    store_raise: bool = False,
) -> tuple[LikeSkill, FakeLiker, FakeAccounts | None, FakeStore]:
    picker = FakeAccounts(accounts) if accounts is not None else None
    liker = FakeLiker(outcomes)
    store = FakeStore(raise_on_save=store_raise)
    return LikeSkill(liker=liker, accounts=picker, store=store, machine_id="bot-1"), liker, picker, store


# —— 1. 输入规整 ——

def test_extract_tweet_id() -> None:
    assert extract_tweet_id(POST_URL) == TWEET_ID
    assert extract_tweet_id("https://x.com/i/status/123?t=1") == "123"
    assert extract_tweet_id("https://x.com/computefluxAI") == ""
    assert extract_tweet_id("") == ""


def test_clamp_like_count() -> None:
    assert clamp_like_count(None) == DEFAULT_LIKE_COUNT
    assert clamp_like_count("3") == 3          # HTTP 里数字以字符串到达时同样可用
    assert clamp_like_count(0) == DEFAULT_LIKE_COUNT
    assert clamp_like_count(-5) == DEFAULT_LIKE_COUNT
    assert clamp_like_count(MAX_LIKE_COUNT) == MAX_LIKE_COUNT
    assert clamp_like_count(MAX_LIKE_COUNT + 100) == MAX_LIKE_COUNT
    assert clamp_like_count("abc") == DEFAULT_LIKE_COUNT


def test_post_url_without_id_fails_cleanly() -> None:
    """取不到 tweet id：ok=false + attempts=0 + 一次点赞都不发起。"""
    skill, liker, _p, store = _skill([True])
    out = _run(skill.run("https://x.com/computefluxAI"))
    assert out["ok"] is False
    assert out["attempts"] == 0
    assert out["success_count"] == 0
    assert out["results"] == []
    assert "cannot extract tweet id" in out["reason"]
    assert liker.calls == []
    # 全失败 → error_logs 留痕（board 错误日志页可见）
    assert len(store.errors) == 1 and store.errors[0][0] == "skill.like"


# —— 2/3. 输出形状与 ok 语义 ——

def test_output_matches_skill_schema() -> None:
    """成功：输出字段与 skill.json 的 output_schema 逐字段对齐。"""
    skill, liker, picker, store = _skill([True], [Account(1), Account(2)])
    out = _run(skill.run(POST_URL, 2))

    schema_fields = {"ok", "reason", "tweet_id", "attempts", "success_count", "results"}
    # account_ids 是内核附带的账号去重信息（schema 允许附加属性），其余必须逐字段对齐
    assert set(out) == schema_fields | {"account_ids"}
    assert out["account_ids"] == [1, 2]
    assert out["ok"] is True and out["reason"] == ""
    assert out["tweet_id"] == TWEET_ID
    assert out["attempts"] == 2 and out["success_count"] == 2
    assert out["results"] == [
        {"tweet_id": TWEET_ID, "success": True, "error": ""},
        {"tweet_id": TWEET_ID, "success": True, "error": ""},
    ]
    assert len(liker.calls) == 2
    # 逐条落库 like_records（成功与失败都记）
    assert [(r[0], r[1]) for r in store.records] == [(TWEET_ID, True), (TWEET_ID, True)]
    assert store.errors == []


def test_partial_success_ok_true() -> None:
    """两次尝试一成一败：ok=true（success_count>0），失败原因保留在 results 里。"""
    skill, _liker, _p, store = _skill([False, True], [Account(1), Account(2)])
    out = _run(skill.run(POST_URL, 2))
    assert out["ok"] is True
    assert out["success_count"] == 1 and out["attempts"] == 2
    assert [r["success"] for r in out["results"]] == [False, True]
    assert store.errors == []  # 有成功就不算技能失败


def test_all_fail_ok_false_with_reason_and_error_log() -> None:
    """全部尝试失败：ok=false + reason 带首条失败原因 + error_logs 留痕。"""
    skill, _liker, _p, store = _skill([False], [Account(1), Account(2)])
    out = _run(skill.run(POST_URL, 2))
    assert out["ok"] is False
    assert out["success_count"] == 0 and out["attempts"] == 2
    assert out["reason"] == "like button not found"
    assert len(store.errors) == 1
    _src, msg, ctx = store.errors[0]
    assert TWEET_ID in msg
    assert json.loads(ctx)["machine_id"] == "bot-1"


def test_like_count_default_is_one() -> None:
    skill, liker, _p, _s = _skill([True], [Account(1)])
    out = _run(skill.run(POST_URL))
    assert out["attempts"] == 1 and len(liker.calls) == 1


# —— 4/5. 账号池行为 ——

def test_accounts_rotated_and_marked_used() -> None:
    """like_count=3：三次用三个不同账号，且每个用完立刻 mark_used。"""
    accounts = [Account(1), Account(2), Account(3)]
    skill, _liker, picker, _s = _skill([True, True, True], accounts)
    out = _run(skill.run(POST_URL, 3))
    assert out["success_count"] == 3
    assert picker is not None
    assert picker.marked == [1, 2, 3]
    # 第 2 次排除 {1}，第 3 次排除 {1,2}（账号级去重）
    assert picker.picked_excluded == [set(), {1}, {1, 2}]


def test_excluded_account_ids_respected() -> None:
    """跨调用去重：excluded_account_ids 里的账号不再被选中。"""
    accounts = [Account(1), Account(2)]
    skill, _liker, picker, _s = _skill([True], accounts)
    out = _run(skill.run(POST_URL, 1, excluded_account_ids={1}))
    assert out["ok"] is True
    assert picker is not None and picker.marked == [2]


def test_no_eligible_account_still_likes_without_cookies() -> None:
    """账号池空（全在冷却/已点过）：不崩，按无登录态发起点赞并如实回报失败。"""
    skill, liker, picker, store = _skill([False], [])
    out = _run(skill.run(POST_URL, 1))
    assert out["ok"] is False and out["attempts"] == 1
    assert liker.calls and liker.calls[0][1] is None
    assert picker is not None and picker.marked == []
    assert len(store.records) == 1


def test_account_cookies_passed_to_liker() -> None:
    """账号登录态随点赞下发：cookies 带 auth_token/ct0（多账号轮换的前提）。"""
    skill, liker, _p, _s = _skill([True], [Account(7)])
    _run(skill.run(POST_URL, 1))
    cookies = liker.calls[0][1]
    assert {c["name"]: c["value"] for c in cookies} == {"auth_token": "tok7", "ct0": "ct0-7"}


# —— 6. 执行器异常不冒泡 ——

def test_like_crash_recorded_as_failed_attempt() -> None:
    """第一次点赞抛异常：记成失败尝试（error 带原因），第二次照常成功。"""
    boom = RuntimeError("browser died")
    skill, liker, picker, _s = _skill([boom, True], [Account(1), Account(2)])
    out = _run(skill.run(POST_URL, 2))
    assert out["ok"] is True and out["success_count"] == 1 and out["attempts"] == 2
    assert out["results"][0]["success"] is False
    assert "like attempt crashed" in out["results"][0]["error"]
    assert len(liker.calls) == 2
    assert picker is not None and picker.marked == [1, 2]


def test_account_pool_crash_does_not_break_skill() -> None:
    """账号池取用抛异常 → 降级为无账号点赞，技能不抛栈。"""

    class BoomAccounts(FakeAccounts):
        def pick_account(self, excluded: set[int] | None = None) -> Any:
            raise RuntimeError("mysql down")

    liker = FakeLiker([True])
    store = FakeStore()
    skill = LikeSkill(liker=liker, accounts=BoomAccounts([]), store=store)
    out = _run(skill.run(POST_URL, 1))
    assert out["ok"] is True and out["success_count"] == 1


# —— 7/8. 落库与副作用 ——

def test_store_failure_swallowed() -> None:
    """落库抛异常（MySQL 瞬断）：吞成日志，点赞结果照常回报。"""
    skill, _liker, _p, store = _skill([True], [Account(1)], store_raise=True)
    out = _run(skill.run(POST_URL, 1))
    assert out["ok"] is True and out["success_count"] == 1
    assert store.records == []


def test_run_batch_likes_each_post_url() -> None:
    """批量入口：逐条按各自 post_url 点赞，条目内 like_count 优先。"""
    skill, liker, _p, store = _skill([True], [Account(1)])
    out = _run(skill.run_batch([
        {"post_url": "https://x.com/a/status/1"},
        {"post_url": "https://x.com/b/status/2", "like_count": 2},
    ]))
    assert [o["tweet_id"] for o in out] == ["1", "2"]
    assert [o["attempts"] for o in out] == [1, 2]
    assert [o["ok"] for o in out] == [True, True]
    assert [c[0] for c in liker.calls] == ["1", "2", "2"]
    assert len(store.records) == 3


def test_run_batch_isolated_failures() -> None:
    """批量里一条全失败：该条 ok=false 并留痕，不影响其它条目。"""
    skill, _liker, _p, store = _skill([False], [Account(1)])
    out = _run(skill.run_batch([
        {"post_url": "https://x.com/a/status/1"},
        {"post_url": "not-a-tweet-url"},
    ]))
    assert [o["ok"] for o in out] == [False, False]
    assert len(store.errors) == 2


if __name__ == "__main__":
    failures = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"PASS {name}")
            except Exception as exc:  # noqa: BLE001
                failures += 1
                import traceback

                print(f"FAIL {name}: {exc}")
                traceback.print_exc()
    sys.exit(1 if failures else 0)
