"""M1-4 单元测试：登录结果回报 social 账号池（工作项 wi-1795685173-65a0）。

覆盖 services/social_report.py（回报桥本体）与 AccountManagerReporter 的双回报接线：
board 账号管理页（core-ui /content/accounts）点「触发登录」→ social 建 login 任务，
worker 消费执行登录后必须把结果 PATCH 回 social，账号状态机才会推进；这一路没接上，
页面就会永远停在「需登录」。

CI 安全：不联网 —— HTTP 全部走注入的 opener（鸭子对象），不发一个真请求。

用法（在 pyreplay 目录下）：
    uv run python3 tests/test_social_report.py          # 无 pytest 时的直跑契约
    uv run pytest tests/test_social_report.py -q        # 有 pytest 时
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
from pathlib import Path
from typing import Any

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from adapters.x_login import AccountManagerReporter, LoginResult  # noqa: E402
from services.social_report import ENV_REPORT_URL, report_social_login  # noqa: E402


class FakeResp:
    """urlopen 返回值的鸭子替身（context manager + status + read）。"""

    def __init__(self, status: int = 200, body: bytes = b"{}") -> None:
        self.status = status
        self._body = body

    def read(self, *_a: Any) -> bytes:
        return self._body

    def getcode(self) -> int:
        return self.status

    def __enter__(self) -> FakeResp:
        return self

    def __exit__(self, *_a: Any) -> None:
        return None


class FakeOpener:
    """记录请求的 urlopen 替身；raise_exc 非空时按异常抛出（模拟网络/HTTP 错误）。"""

    def __init__(self, resp: FakeResp | None = None, raise_exc: Exception | None = None) -> None:
        self.requests: list[Any] = []
        self.timeouts: list[Any] = []
        self.resp = resp or FakeResp()
        self.raise_exc = raise_exc

    def __call__(self, req: Any, timeout: Any = None) -> FakeResp:
        self.requests.append(req)
        self.timeouts.append(timeout)
        if self.raise_exc is not None:
            raise self.raise_exc
        return self.resp


class DuckCursor:
    """鸭子游标：SELECT 永远查不到行（账号不在 x_accounts），INSERT 视为建行。"""

    def __init__(self, rows: list[dict[str, Any]]) -> None:
        self._rows = rows

    def execute(self, sql: str, params: tuple[Any, ...] = ()) -> None:
        self._rows.append({"sql": " ".join(sql.split()), "params": params})

    def fetchone(self) -> dict[str, Any] | None:
        return None

    def __enter__(self) -> DuckCursor:
        return self

    def __exit__(self, *_a: Any) -> None:
        return None


class DuckConn:
    def __init__(self, rows: list[dict[str, Any]]) -> None:
        self._rows = rows

    def cursor(self) -> DuckCursor:
        return DuckCursor(self._rows)


def _http_error(code: int, body: bytes = b'{"error":"account not found"}') -> urllib.error.HTTPError:
    import io

    return urllib.error.HTTPError(
        "http://social:8010/api/social/accounts/x/alice/login", code, "error", {}, io.BytesIO(body)
    )


def test_skips_without_config() -> None:
    """未配置回报地址 → skipped 且一个请求都不发（登录链行为与 M1-3 完全一致）。"""
    os.environ.pop(ENV_REPORT_URL, None)
    opener = FakeOpener()
    status = report_social_login("x", "alice", True, "ok", opener=opener)
    assert status.startswith("skipped")
    assert opener.requests == []


def test_skips_on_missing_identity() -> None:
    """channel / account_id 为空 → 不拼畸形 URL，直接 skipped。"""
    opener = FakeOpener()
    assert report_social_login("", "alice", True, opener=opener).startswith("skipped")
    assert report_social_login("x", "  ", True, opener=opener).startswith("skipped")
    assert opener.requests == []


def test_success_patches_login_endpoint_with_escaped_path() -> None:
    """成功回报：PATCH 到登录回报端点，路径段百分号编码，body 是 {success, message}。"""
    opener = FakeOpener()
    status = report_social_login("x", "张三 user", True, "登录成功", base_url="http://social:8010/", opener=opener)
    assert status == "updated"
    assert len(opener.requests) == 1
    req = opener.requests[0]
    expected = "http://social:8010/api/social/accounts/x/%E5%BC%A0%E4%B8%89%20user/login"
    assert req.full_url == expected, req.full_url
    assert req.get_method() == "PATCH"
    body = json.loads(req.data.decode("utf-8"))
    assert body == {"success": True, "message": "登录成功"}
    assert req.get_header("Content-type") == "application/json"


def test_failure_reports_message() -> None:
    """失败回报：success=false + 原因原样带给 social（social 侧 loginreport 判定状态）。"""
    opener = FakeOpener()
    status = report_social_login("x", "alice", False, "2FA required", base_url="http://social:8010", opener=opener)
    assert status == "updated"
    body = json.loads(opener.requests[0].data.decode("utf-8"))
    assert body == {"success": False, "message": "2FA required"}


def test_network_error_never_raises() -> None:
    """网络抖动 → 吞成 failed 状态串，绝不冒泡（回报失败不许打崩登录链）。"""
    opener = FakeOpener(raise_exc=OSError("connection refused"))
    status = report_social_login("x", "alice", True, base_url="http://social:8010", opener=opener)
    assert status.startswith("failed")


def test_http_error_reports_code_and_detail() -> None:
    """social 明确拒收（404 账号不存在）→ failed + HTTP 码 + social 的 error 文本。"""
    opener = FakeOpener(raise_exc=_http_error(404))
    status = report_social_login("x", "ghost", True, base_url="http://social:8010", opener=opener)
    assert status.startswith("failed: HTTP 404")
    assert "account not found" in status


def test_reporter_double_reports_to_social() -> None:
    """AccountManagerReporter 双回报：x_accounts 状态为前缀，social 状态以 ; social: 后缀。"""
    calls: list[tuple[str, str, bool, str]] = []

    def fake_report_social(channel: str, account_id: str, success: bool, message: str = "") -> str:
        calls.append((channel, account_id, success, message))
        return "updated"

    import services.social_report as social_report

    # 模块属性打桩：AccountManagerReporter.report 经 `from services import social_report`
    # + 属性调用解析，这里替换后测试里不会发出任何真请求。
    saved = social_report.report_social_login
    social_report.report_social_login = fake_report_social  # type: ignore[assignment]

    conn_rows: list[dict[str, Any]] = []
    os.environ["MYSQL_HOST"] = "127.0.0.1"
    try:
        reporter = AccountManagerReporter(machine_id="bot-1")
        reporter._connect = lambda: DuckConn(conn_rows)  # 接缝注入（不连真库）
        # 账号不存在 + 成功 → x_accounts 一路 created；social 一路 updated → 拼接
        status = reporter.report(LoginResult(account="alice", success=True, username="alice"))
        assert status == "created; social:updated"
        assert calls == [("x", "alice", True, "login failed")]

        # 失败回报把原因带给 social（social 侧据此落 need_login 并记 last_login_error）
        calls.clear()
        status = reporter.report(LoginResult(account="bob", success=False, message="bad password"))
        assert status.startswith("skipped: account bob not registered")
        assert calls == [("x", "bob", False, "bad password")]
    finally:
        social_report.report_social_login = saved  # type: ignore[assignment]
        os.environ.pop("MYSQL_HOST", None)


def test_reporter_unchanged_when_social_unconfigured() -> None:
    """未配置 SOCIAL_LOGIN_REPORT_URL → 状态串与 M1-3 完全一致（无 ; social: 后缀）。"""
    os.environ.pop(ENV_REPORT_URL, None)
    os.environ["MYSQL_HOST"] = "127.0.0.1"

    def _no_network(*_a: Any, **_k: Any) -> Any:
        raise AssertionError("未配置回报地址时绝不允许发出网络请求")

    import urllib.request as _ur

    saved_open = _ur.urlopen
    _ur.urlopen = _no_network  # type: ignore[assignment]
    try:
        reporter = AccountManagerReporter(machine_id="bot-1")
        reporter._connect = lambda: DuckConn([])  # type: ignore[assignment]
        status = reporter.report(LoginResult(account="alice", success=True, username="alice"))
        assert status == "created"
    finally:
        _ur.urlopen = saved_open  # type: ignore[assignment]
        os.environ.pop("MYSQL_HOST", None)


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
