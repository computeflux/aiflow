"""M1-3 单元测试：XAdapter.login 与登录会话持久化（工作项 wi-1795586435-e694）。

CI 安全：不起浏览器、不连 MySQL、不联网。浏览器与账号池都走注入接缝
（``XLoginHooks`` / ``PageOps`` / 鸭子类型 AccountManager），MySQL 未配置时
回报器返回 ``skipped``，所以整份测试在任何干净环境都能跑。

用法（在 pyreplay 目录下）：
    uv run python3 tests/test_x_login.py          # 无 pytest 时的直跑契约
    uv run pytest tests/test_x_login.py -q        # 有 pytest 时

覆盖的行为点（对应任务验收标准）：
    1. 账号标识安全化 + profile/session 路径按账号隔离（含路径穿越防御）；
    2. 登录态文件读写往返 + 权限 0600；
    3. 已有有效登录态时复用（不起浏览器，source=reuse）+ force 强制重登；
    4. 账密登录成功：从 cookies 取 auth_token/ct0 → 落盘 → 回报 AccountManager；
    5. 2FA：有码则填入；无 handler 时 needs_manual=True / intervention=two_factor；
    6. 扫码：占位接口报介入并落盘二维码路径；装了回调 handler 则记录介入；
    7. 登录失败（未过登录墙 / 缺 cookies）不写会话文件，失败原因回报；
    8. LoginResult.summary() 脱敏（不含 auth_token/ct0 明文）；
    9. AccountManagerReporter：MySQL 未配置 = skipped；鸭子连接上 updated/created 幂等；
   10. XAdapter 门面：headless 开关传递、介入 handler、会话读写方法。
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import stat
import sys
import tempfile
import types
from pathlib import Path
from typing import Any

try:
    import pytest
except ModuleNotFoundError:  # dependency-less sandbox (CI contract fallback)
    _pytest = types.ModuleType("pytest")

    def _fixture(func=None, **_k):
        if func is not None:
            return func
        return lambda f: f

    _pytest.fixture = _fixture  # type: ignore[attr-defined]
    pytest = _pytest  # type: ignore[assignment]

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from adapters.x_adapter import XAdapter, make_intervention  # noqa: E402
from adapters.x_login import (  # noqa: E402
    AccountManagerReporter,
    InterventionRequired,
    LoginResult,
    NoIntervention,
    PageOps,
    XLoginConfig,
    XLoginFlow,
    XLoginHooks,
    XLoginOutcome,
    XSession,
    load_session,
    mask_secret,
    profile_dir_for,
    sanitize_account_id,
    save_session,
    session_file_for,
)  # noqa: E402

AUTH_TOKEN = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
CT0 = "00112233445566778899aabbccddeeff0123456789abcdef0123456789abcdef"


# —— fixtures（pytest 与直跑两种模式都能用：参数可显式传入）——


def isolated_roots(tmp_path: Path | None = None) -> dict[str, Any]:
    """把 profile/session 根目录指向临时目录，并清掉 MySQL 配置（回报器走 skipped）。

    同时作为 pytest fixture 与直跑契约的普通函数（pytest 注入 tmp_path，
    直跑时由 _main() 传 tempfile 目录）。
    """
    base = tmp_path or Path(tempfile.mkdtemp(prefix="pyreplay-x-login-"))
    profile_dir = base / "p"
    session_dir = base / "s"
    os.environ["X_PROFILE_DIR"] = str(profile_dir)
    os.environ["X_SESSION_DIR"] = str(session_dir)
    os.environ.pop("MYSQL_HOST", None)
    os.environ["MACHINE_ID"] = "test-bot"
    return {"tmp": base, "profiles": profile_dir, "sessions": session_dir}
isolated_roots = pytest.fixture(isolated_roots)

# —— fake 浏览器接缝 ——


class FakePage:
    """最小 fake page：记录动作、按脚本给出 cookies / 元素存在性。"""

    def __init__(
        self,
        cookies: list[dict[str, Any]] | None = None,
        present: set[str] | None = None,
        screenshot: bytes = b"\x89PNG-fake",
        raise_on: str = "",
    ) -> None:
        self.cookies_list = cookies if cookies is not None else []
        self.selectors = present or set()
        self._screenshot = screenshot
        self.raise_on = raise_on
        self.actions: list[tuple[str, str]] = []
        self.url = ""

    async def goto(self, url: str, **_kw: Any) -> None:
        self.url = url
        self.actions.append(("goto", url))

    def _check(self, what: str) -> None:
        """raise_on 支持两种粒度：精确名（"fill:input[name='password']"）或动作类（"fill:"）。"""
        if not self.raise_on:
            return
        rule = self.raise_on
        if rule == what or (rule.endswith(":") and what.startswith(rule)):
            raise RuntimeError(f"boom: {what}")

    async def fill(self, selector: str, value: str, **_kw: Any) -> None:
        self._check(f"fill:{selector}")
        self.actions.append(("fill", f"{selector}={value}"))

    async def click(self, selector: str, **_kw: Any) -> None:
        self._check(f"click:{selector}")
        self.actions.append(("click", selector))

    async def press(self, selector: str, key: str, **_kw: Any) -> None:
        """X 登录每步表单用 Enter 提交（真 Playwright 是 page.press(selector, key)）。"""
        self._check(f"press:{selector}")
        self.actions.append(("press", f"{selector}:{key}"))

    async def wait_for(self, selector: str, **_kw: Any) -> None:
        # 任一子选择器命中 selectors 集合即算存在（真 Playwright 用逗号选择器）
        for part in (part.strip() for part in selector.split(",")):
            if part in self.selectors:
                self.actions.append(("wait", selector))
                return
        raise TimeoutError(f"selector not found: {selector}")

    async def present(self, selector: str, timeout_ms: int = 0) -> bool:
        """有界探测（与 PageOps._present 同语义：命中 True / 未命中 False）。"""
        for part in (part.strip() for part in selector.split(",")):
            if part in self.selectors:
                self.actions.append(("present", selector))
                return True
        self.actions.append(("present-miss", selector))
        return False

    async def cookies(self) -> list[dict[str, Any]]:
        return list(self.cookies_list)

    async def screenshot(self, selector: str | None = None, **_kw: Any) -> bytes:
        self.actions.append(("shot", selector or "*page*"))
        return self._screenshot


def _ops(page: FakePage) -> PageOps:
    """fake page → 登录链的动词接缝（签名与 PageOps.for_timeout 包装真 Page 后一致）。"""
    return PageOps(
        goto=page.goto,
        fill=page.fill,
        click=page.click,
        press=page.press,
        wait_for=page.wait_for,
        present=page.present,
        cookies=page.cookies,
        screenshot=page.screenshot,
    )


def _flow(
    page: FakePage,
    roots: dict[str, Any],
    intervention: Any = None,
    account_manager: Any = None,
    config: dict[str, Any] | None = None,
) -> XLoginFlow:
    """构造一个全接缝注入的登录链（不碰真浏览器 / 真 MySQL）。"""

    async def _launch(profile_dir: Path, headless: bool, chrome_bin: str) -> FakePage:
        page.actions.append(("launch", f"{profile_dir}|headless={headless}|{chrome_bin}"))
        page.profile_dir = profile_dir  # type: ignore[attr-defined]
        return page

    async def _teardown(_page: Any) -> None:
        page.actions.append(("teardown", ""))

    cfg = XLoginConfig.from_config(config or {}, headless=True)
    return XLoginFlow(
        config=cfg,
        intervention=intervention,
        hooks=XLoginHooks(launch=_launch, teardown=_teardown),
        ops=_ops(page),
        account_manager=account_manager,
    )


LOGGED_IN = {"[data-testid=\"AppTabBar_Profile-Link\"]"}
LOGIN_FORM = {"input[name='username']", "input[name='password']"}
QRCODE = {"canvas"}
CODE_BOX = "input[name='text-code']"


def _login_cookies() -> list[dict[str, Any]]:
    return [
        {"name": "auth_token", "value": AUTH_TOKEN},
        {"name": "ct0", "value": CT0},
        {"name": "username", "value": "alice"},
    ]


def _run(coro: Any) -> Any:
    return asyncio.run(coro)


# —— 1. 账号隔离与路径安全 ——


def test_sanitize_and_paths_isolated(isolated_roots: dict[str, Any]) -> None:
    assert sanitize_account_id("alice@example.com") == "alice_example_com"
    assert sanitize_account_id("  ") == "_"

    a = profile_dir_for("alice")
    b = profile_dir_for("bob")
    assert a != b, "profile 目录必须按账号隔离"
    assert a.parent == isolated_roots["profiles"]
    assert session_file_for("alice").parent == isolated_roots["sessions"]

    # 路径穿越防御：目录名里不允许出现分隔符 / ".."
    for evil in ("../../etc/passwd", "..", "/etc/shadow", "a/../../b"):
        name = sanitize_account_id(evil)
        assert "/" not in name and ".." not in name, f"{evil!r} → {name!r}"
        assert profile_dir_for(evil).parent == isolated_roots["profiles"]
        assert session_file_for(evil).parent == isolated_roots["sessions"]


# —— 2. 登录态读写 + 权限 ——


def test_session_roundtrip_permissions(isolated_roots: dict[str, Any]) -> None:
    assert load_session("nobody") is None

    path = save_session("alice", XSession(auth_token=AUTH_TOKEN, ct0=CT0, username="alice"))
    assert path.exists()
    mode = stat.S_IMODE(path.stat().st_mode)
    assert mode == 0o600, f"登录态文件权限应为 0600，实际 {oct(mode)}"

    got = load_session("alice")
    assert got is not None and got.valid
    assert got.auth_token == AUTH_TOKEN and got.ct0 == CT0 and got.username == "alice"

    # cookies() 形状（Playwright / crawl4ai 可用）
    cookies = got.cookies()
    names = {c["name"] for c in cookies}
    assert names == {"auth_token", "ct0"}
    assert all(c["domain"] == ".x.com" for c in cookies)

    # 损坏文件不抛，返回 None（登录链自行决定重登）
    path.write_text("{not json", encoding="utf-8")
    assert load_session("alice") is None


# —— 3. 复用已持久化登录态 ——


def test_reuse_persisted_session(isolated_roots: dict[str, Any]) -> None:
    save_session("alice", XSession(auth_token=AUTH_TOKEN, ct0=CT0, username="alice"))

    page = FakePage()
    flow = _flow(page, isolated_roots)
    result = _run(flow.login("alice"))

    assert result.success and result.source == "reuse"
    assert result.auth_token == AUTH_TOKEN
    assert not any(a[0] == "launch" for a in page.actions), "复用登录态不得起浏览器"
    # 复用的会话同样要回报 AccountManager（MySQL 未配置 → skipped，不阻断）
    assert result.account_manager.startswith("skipped")


def test_force_relogins_even_with_session(isolated_roots: dict[str, Any]) -> None:
    save_session("alice", XSession(auth_token=AUTH_TOKEN, ct0=CT0, username="alice"))

    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN)
    flow = _flow(page, isolated_roots)
    result = _run(flow.login("alice", "pw", force=True))

    assert result.success and result.source == "password"
    assert any(a[0] == "launch" for a in page.actions), "force 必须重走登录流程"


# —— 4. 账密登录成功：取 cookies → 落盘 → 回报 ——


def test_password_login_success_persists_and_reports(isolated_roots: dict[str, Any]) -> None:
    saved: list[dict[str, Any]] = []

    class Manager:
        def report_login(self, result: LoginResult) -> str:
            saved.append(result.summary())
            return "updated"

    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN)
    flow = _flow(page, isolated_roots, account_manager=Manager())
    _ = isolated_roots  # fixture 已把 profile/session 根目录指到临时目录
    result = _run(flow.login("alice@example.com", "s3cret-pw"))

    assert result.success, result.message
    assert result.source == "password"
    assert result.account_manager == "updated" and saved and saved[0]["success"] is True

    # 按账号隔离落盘：session 文件里就是刚取到的登录态
    session = load_session("alice@example.com")
    assert session is not None and session.valid
    assert session.auth_token == AUTH_TOKEN and session.ct0 == CT0
    assert Path(result.session_file) == session_file_for("alice@example.com")
    assert Path(result.profile_dir) == profile_dir_for("alice@example.com")

    # 浏览器生命周期：launch → 填用户名/密码并提交 → teardown（失败路径同样收口）
    kinds = [a[0] for a in page.actions]
    assert kinds[0] == "launch" and kinds[-1] == "teardown"
    # 选择器是「多候选逗号串」（autocomplete / name / aria-label 兜底），所以断言
    # 「该字段的选择器组里含 name=...」+ 值，而不是断言整串前缀。
    fills = [a[1] for a in page.actions if a[0] == "fill"]
    assert any("input[name='username']" in f and f.endswith("=alice@example.com") for f in fills)
    assert any("input[name='password']" in f and f.endswith("=s3cret-pw") for f in fills)
    assert any(
        a[0] == "press" and "input[name='password']" in a[1] and a[1].endswith(":Enter")
        for a in page.actions
    )


def test_teardown_runs_when_page_action_raises(isolated_roots: dict[str, Any]) -> None:
    # raise_on="fill:" = 任何 fill 动作都炸（选择器是多候选逗号串，别硬写整串）
    page = FakePage(present=LOGIN_FORM, raise_on="fill:")
    flow = _flow(page, isolated_roots)
    result = _run(flow.login("alice", "pw"))

    assert not result.success and not result.needs_manual
    assert "boom" in result.message
    assert page.actions[-1][0] == "teardown", "登录失败也必须收掉浏览器"
    assert load_session("alice") is None, "登录失败不得写会话文件"


# —— 5. 2FA 人工介入 ——


def test_two_factor_with_provided_code(isolated_roots: dict[str, Any]) -> None:
    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN | {CODE_BOX})
    flow = _flow(page, isolated_roots)
    result = _run(flow.login("alice", "pw", two_factor_code="123456"))

    assert result.success and result.source == "manual" and result.intervention == "two_factor"
    # 介入已闭环：intervention 记下「页面要求过 2FA」，needs_manual 必须为 False，
    # 否则 /login 会同时给出 ok=true 与 needs_manual=true，board 会反复带码重登。
    assert not result.needs_manual, "2FA 码提交成功后不得再标 needs_manual"
    fills = [a[1] for a in page.actions if a[0] == "fill"]
    assert any("text-code" in f and f.endswith("=123456") for f in fills)


def test_two_factor_without_handler_reports_manual(isolated_roots: dict[str, Any]) -> None:
    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN | {CODE_BOX})
    flow = _flow(page, isolated_roots, intervention=NoIntervention())
    result = _run(flow.login("alice", "pw"))

    assert not result.success
    assert result.needs_manual and result.intervention == "two_factor"
    assert load_session("alice") is None, "介入未完成不得写会话文件"

    # 装了 2FA 回调 handler：取到码即登录成功，来源标 manual
    page2 = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN | {CODE_BOX})
    flow2 = _flow(
        page2,
        isolated_roots,
        intervention=make_intervention(two_factor=lambda acct, hint: "654321"),
    )
    result2 = _run(flow2.login("alice", "pw"))
    assert result2.success and result2.source == "manual" and result2.intervention == "two_factor"


def test_two_factor_invalid_code_shape_rejected(isolated_roots: dict[str, Any]) -> None:
    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN | {CODE_BOX})
    flow = _flow(page, isolated_roots, config={"login_2fa_code_max_len": 6})
    result = _run(flow.login("alice", "pw", two_factor_code="12345678901234567890"))

    assert not result.success and result.intervention == "two_factor"
    assert "invalid 2FA code shape" in result.message


# —— 6. 扫码登录人工介入 ——


def test_qrcode_intervention_placeholder(isolated_roots: dict[str, Any]) -> None:
    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN | QRCODE)
    flow = _flow(page, isolated_roots, intervention=NoIntervention())
    result = _run(flow.login("alice", "pw"))

    assert not result.success
    assert result.needs_manual and result.intervention == "qrcode"
    assert result.qrcode_path.endswith("login-qrcode.png")
    assert Path(result.qrcode_path).parent == profile_dir_for("alice")


def test_qrcode_file_written_before_handler_is_called(isolated_roots: dict[str, Any]) -> None:
    """顺序红线：二维码文件必须在 handler **被调用时已经存在**。

    扫码介入的 handler 通常阻塞等人在手机上扫码（或把码推给 board / Matrix）；
    先叫 handler 后写盘 = 介入方拿到一个还不存在的路径，人工永远扫不上。
    """
    observed: list[bool] = []

    def on_qr(_account: str, png_path: str) -> None:
        observed.append(Path(png_path).is_file())

    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN | QRCODE)
    flow = _flow(page, isolated_roots, intervention=make_intervention(qrcode=on_qr))
    result = _run(flow.login("alice", "pw"))

    assert observed == [True]
    assert result.qrcode_path and Path(result.qrcode_path).is_file()


def test_qrcode_returns_no_path_when_screenshot_fails(isolated_roots: dict[str, Any]) -> None:
    """截图失败 → 不返回一个不存在的路径（宁可不给码，也不给假码）。"""
    seen: list[str] = []

    def on_qr(_account: str, png_path: str) -> None:
        seen.append(png_path)

    page = FakePage(
        cookies=[],  # 没拿到码 → 也没登录态：登录必须失败且要求人工介入
        present=LOGIN_FORM | LOGGED_IN | QRCODE,
        screenshot=b"",  # 空字节 = 截图失败
    )
    flow = _flow(page, isolated_roots, intervention=make_intervention(qrcode=on_qr))
    result = _run(flow.login("alice", "pw"))

    assert result.needs_manual and result.intervention == "qrcode"
    assert result.qrcode_path == ""
    assert seen == []  # 没码可给就不叫 handler


def test_qrcode_handler_completes_login(isolated_roots: dict[str, Any]) -> None:
    seen: list[str] = []

    def on_qr(account: str, png_path: str) -> None:
        seen.append(f"{account}:{png_path}")

    page = FakePage(
        cookies=_login_cookies(),
        present=LOGIN_FORM | LOGGED_IN | QRCODE,
        screenshot=b"\x89PNGfake-qr",
    )
    flow = _flow(page, isolated_roots, intervention=make_intervention(qrcode=on_qr))
    result = _run(flow.login("alice", "pw"))

    assert seen and seen[0].startswith("alice:")
    assert result.success and result.source == "manual" and result.intervention == "qrcode"
    # 二维码落盘（0600，登录态材料）
    qr = Path(result.qrcode_path)
    assert qr.read_bytes() == b"\x89PNGfake-qr"
    assert stat.S_IMODE(qr.stat().st_mode) == 0o600


# —— 7. 登录失败语义 ——


def test_login_wall_not_passed(isolated_roots: dict[str, Any]) -> None:
    page = FakePage(cookies=[], present=LOGIN_FORM)  # 无登录落地元素、无 cookies
    flow = _flow(page, isolated_roots)
    result = _run(flow.login("alice", "pw"))

    assert not result.success
    assert "login wall not passed" in result.message
    assert load_session("alice") is None


def test_logged_in_without_session_cookies(isolated_roots: dict[str, Any]) -> None:
    page = FakePage(cookies=[{"name": "ct0", "value": CT0}], present=LOGIN_FORM | LOGGED_IN)
    flow = _flow(page, isolated_roots)
    result = _run(flow.login("alice", "pw"))

    assert not result.success
    assert "auth_token/ct0 cookies missing" in result.message
    assert load_session("alice") is None


def test_missing_account_and_password(isolated_roots: dict[str, Any]) -> None:
    page = FakePage()
    flow = _flow(page, isolated_roots)

    empty_account = _run(flow.login("   "))
    assert not empty_account.success and "account is required" in empty_account.message

    no_password = _run(flow.login("alice"))
    assert not no_password.success and no_password.needs_manual
    assert no_password.intervention == "password"
    assert not any(a[0] == "launch" for a in page.actions), "缺密码时不该起浏览器"


# —— 8. 脱敏 ——


def test_summary_masks_secrets(isolated_roots: dict[str, Any]) -> None:
    result = LoginResult(
        account="alice", success=True, auth_token=AUTH_TOKEN, ct0=CT0, username="alice"
    )
    summary = json.dumps(result.summary(), ensure_ascii=False)
    assert AUTH_TOKEN not in summary and CT0 not in summary
    assert mask_secret(AUTH_TOKEN) not in (AUTH_TOKEN,)
    assert mask_secret("") == ""
    assert mask_secret("short") == "*****"


# —— 9. AccountManagerReporter ——


class FakeCursor:
    def __init__(self, conn: FakeConn) -> None:
        self.conn = conn
        self._rows: list[dict[str, Any]] = []

    def execute(self, sql: str, params: tuple[Any, ...] = ()) -> None:
        self.conn.calls.append((" ".join(sql.split()), params))
        if sql.strip().upper().startswith("SELECT"):
            machine, username = params
            self._rows = [
                {"id": r["id"]}
                for r in self.conn.rows
                if r["machine_id"] == machine and r["username"] == username
            ]
        elif sql.strip().upper().startswith("INSERT"):
            cols = (
                "machine_id",
                "username",
                "auth_token",
                "ct0",
                "daily_like_limit",
                "cooldown_minutes",
            )
            self.conn.rows.append({"id": len(self.conn.rows) + 1, **dict(zip(cols, params, strict=True))})
        else:  # UPDATE
            target = self.conn.rows[int(params[-1]) - 1]
            if "auth_token" in sql:
                target["auth_token"], target["ct0"] = params[0], params[1]
            target["last_login_error"] = params[-2] if "auth_token" in sql else params[0]

    def fetchone(self) -> dict[str, Any] | None:
        return self._rows[0] if self._rows else None

    def __enter__(self) -> FakeCursor:
        return self

    def __exit__(self, *_a: Any) -> None:
        return None


class FakeConn:
    def __init__(self) -> None:
        self.rows: list[dict[str, Any]] = []
        self.calls: list[tuple[str, tuple[Any, ...]]] = []

    def cursor(self) -> FakeCursor:
        return FakeCursor(self)


def test_reporter_skips_without_mysql(monkeypatch_env: Any = None) -> None:
    os.environ.pop("MYSQL_HOST", None)
    reporter = AccountManagerReporter(machine_id="bot-1")
    status = reporter.report(LoginResult(account="alice", success=True, username="alice"))
    assert status.startswith("skipped")


def test_reporter_create_then_update() -> None:
    conn = FakeConn()
    os.environ["MYSQL_HOST"] = "127.0.0.1"
    try:
        reporter = AccountManagerReporter(
            defaults={"default_daily_like_limit": 7, "default_cooldown_minutes": 3},
            machine_id="bot-1",
        )
        reporter._connect = lambda: conn  # 接缝注入 fake 连接（不连真库）
        ok = LoginResult(
            account="alice@example.com",
            success=True,
            username="alice",
            auth_token=AUTH_TOKEN,
            ct0=CT0,
        )
        assert reporter.report(ok) == "created"
        assert conn.rows[0]["daily_like_limit"] == 7 and conn.rows[0]["cooldown_minutes"] == 3

        # 幂等：同 machine_id + username 再登录 → 更新原行，不新增
        assert reporter.report(ok) == "updated"
        assert len(conn.rows) == 1
        assert conn.rows[0]["auth_token"] == AUTH_TOKEN

        # 失败登录：只记 last_login_error，不覆盖登录态
        bad = LoginResult(account="alice@example.com", success=False, username="alice", message="bad pw")
        assert reporter.report(bad) == "updated"
        assert conn.rows[0]["last_login_error"] == "bad pw"
        assert conn.rows[0]["auth_token"] == AUTH_TOKEN

        # 别的机器不会命中本机账号（机器隔离口径）
        other = AccountManagerReporter(machine_id="bot-2")
        other._connect = lambda: conn
        assert other.report(ok) == "created"
        assert len(conn.rows) == 2
    finally:
        os.environ.pop("MYSQL_HOST", None)


def test_reporter_never_creates_row_on_failure() -> None:
    """核心红线：账号不存在 + 登录失败 → **绝不 INSERT**。

    ``AccountManager.load_eligible`` 只按 ``enabled = 1 AND machine_id`` 过滤，不看
    token 是否为空。给从没登录成功的账号建一行 enabled=1 的空 auth_token/ct0，
    等于把它塞进点赞池，之后每次挑到都必然运行时失败。
    """
    conn = FakeConn()
    os.environ["MYSQL_HOST"] = "127.0.0.1"
    try:
        reporter = AccountManagerReporter(machine_id="bot-1")
        reporter._connect = lambda: conn  # 接缝注入 fake 连接（不连真库）
        status = reporter.report(
            LoginResult(account="ghost", success=False, message="captcha required")
        )
        assert status.startswith("skipped")
        assert conn.rows == []
        assert not any(c[0].upper().startswith("INSERT") for c in conn.calls)
    finally:
        os.environ.pop("MYSQL_HOST", None)


def test_reporter_sql_uses_only_real_columns() -> None:
    """回报 SQL 只能用 Go ``XAccount`` 模型里真实存在的列。

    列名对不上会在真库抛 1054 Unknown column，并被 except 吞成 ``failed`` ——
    单测碰不到真库，所以在这里把「SQL 里出现的列 ⊆ 模型列」钉住。
    """
    go_model_columns = {
        "id", "machine_id", "username", "auth_token", "ct0", "enabled",
        "daily_like_limit", "cooldown_minutes", "used_today", "last_used_at",
        "last_login_at", "last_login_error", "created_at", "updated_at",
    }
    conn = FakeConn()
    os.environ["MYSQL_HOST"] = "127.0.0.1"
    try:
        reporter = AccountManagerReporter(machine_id="bot-1")
        reporter._connect = lambda: conn
        reporter.report(
            LoginResult(
                account="alice", success=True, username="alice", auth_token=AUTH_TOKEN, ct0=CT0
            )
        )
        reporter.report(
            LoginResult(account="alice", success=False, username="alice", message="boom")
        )
    finally:
        os.environ.pop("MYSQL_HOST", None)

    assert conn.calls, "reporter issued no SQL at all"
    for sql, _params in conn.calls:
        for column in re.findall(r"\b([a-z_]+)\s*=", sql):
            assert column in go_model_columns, f"unknown x_accounts column: {column}"

def test_login_timeout_returns_result_not_exception(isolated_roots: dict[str, Any]) -> None:
    """登录整链超时（挂在页面动作上）→ 返回失败结果，绝不冒泡打崩调用方；会话收口。

    登录跑在 skill 服务线程里：一次挂死 = 这条登录通道一直占着。
    """
    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN)

    async def _launch(profile_dir: Path, headless: bool, chrome_bin: str) -> Any:
        page.actions.append(("launch", str(profile_dir)))
        return page

    async def _teardown(_page: Any) -> None:
        page.actions.append(("teardown", ""))

    flow = XLoginFlow(
        config=XLoginConfig.from_config({"login_timeout_sec": 0.2}, headless=True),
        hooks=XLoginHooks(launch=_launch, teardown=_teardown),
        ops=_ops(page),
    )
    # 导航挂死 → 超时取消落在页面动作上（浏览器已起来，走页面收尾）
    async def _hang_goto(url: str, **_kw: Any) -> None:
        await asyncio.sleep(10)  # 远超 login_timeout_sec

    flow.ops = PageOps(
        goto=_hang_goto,
        fill=page.fill,
        click=page.click,
        press=page.press,
        wait_for=page.wait_for,
        present=page.present,
        cookies=page.cookies,
        screenshot=page.screenshot,
    )

    result = _run(flow.login("alice", "pw"))
    assert not result.success and not result.needs_manual
    assert "timed out" in result.message
    assert page.actions[-1][0] == "teardown", "超时也必须收掉浏览器会话"


def test_login_timeout_during_launch_releases_profile(isolated_roots: dict[str, Any]) -> None:
    """超时打断在「起浏览器」这一步：会话必须就地收口，并走到 profile 释放那一步。

    漏掉这一步的代价不是日志里少一行，而是账号的 user-data-dir 一直留着 Chromium 的
    SingletonLock：下次登录同一个账号直接起不来。释放的**内部** IO（起 driver / 关 context /
    停 driver）由 ``test_release_profile_releases_driver_even_when_browser_missing`` 用真实
    实现覆盖；本用例只证明「取消落在起浏览器上 → 登录链真的调到了释放」。
    """
    opened: list[str] = []

    async def _hang_on_launch(profile_dir: Path, headless: bool, chrome_bin: str) -> Any:
        await asyncio.sleep(10)  # 远超 login_timeout_sec：起浏览器挂死
        raise AssertionError("unreachable")

    async def _release(profile_dir: Path) -> None:
        opened.append(str(profile_dir))

    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN)
    flow = XLoginFlow(
        config=XLoginConfig.from_config({"login_timeout_sec": 0.3}, headless=True),
        hooks=XLoginHooks(launch=_hang_on_launch, teardown=None),
        ops=_ops(page),
    )
    flow._release_profile = _release  # type: ignore[assignment]

    result = _run(flow.login("alice", "pw"))
    assert not result.success and "timed out" in result.message
    assert opened == [str(profile_dir_for("alice"))], "起浏览器阶段被取消也要释放 profile"


def test_release_profile_releases_driver_even_when_browser_missing(monkeypatch: Any) -> None:
    """``_release_profile`` 的真身：起一次性 driver 打开同一 user-data-dir 再关掉。

    不打桩、不跳步：即使目录压根没被占用（起浏览器失败 = 正常情形），也必须走到
    ``pw.stop()``，否则每次释放都漏一个 node driver 进程。
    """
    events: list[str] = []

    class _Context:
        def __init__(self, events: list[str]) -> None:
            self.events = events

        async def close(self) -> None:
            self.events.append("context.close")

    class _Chromium:
        async def launch_persistent_context(self, user_data_dir: str, **_kw: Any) -> Any:
            events.append(f"launch:{Path(user_data_dir).name}")
            return _Context(events)

    pw = _FakePlaywright(events)

    async def _start() -> Any:
        pw.chromium = _Chromium()  # type: ignore[attr-defined]
        return pw

    async_api = types.ModuleType("playwright.async_api")
    async_api.async_playwright = lambda: types.SimpleNamespace(start=_start)  # type: ignore[attr-defined]
    playwright_pkg = types.ModuleType("playwright")
    playwright_pkg.async_api = async_api  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "playwright", playwright_pkg)
    monkeypatch.setitem(sys.modules, "playwright.async_api", async_api)

    flow = XLoginFlow(config=XLoginConfig.from_config({}, headless=True))
    with tempfile.TemporaryDirectory() as td:
        _run(flow._release_profile(Path(td) / "alice"))
    assert events == ["launch:alice", "context.close", "playwright.stop"]


def test_login_timeout_cancellation_awaits_real_teardown(monkeypatch: Any) -> None:
    """整链超时（真收尾，**不打桩** teardown）：取消上下文里也必须把 context + driver 关掉。

    这条专治「登记-补跑通道空转」：收尾任务登记在**驱动登录协程的那个 Task** 上，
    ``login()`` 在 ``wait_for`` 之后取回并跑完。若登记方与 drain 方不是同一个 Task，
    收尾任务只能靠事件循环顺带推进，``asyncio.run`` 退出时被 ``_cancel_all_tasks`` 直接
    cancel —— 这里就会看到 context.close / playwright.stop 没发生。
    """
    events: list[str] = []

    class _Context:
        async def close(self) -> None:
            await asyncio.sleep(0.01)  # 真收尾必然有挂起点
            events.append("context.close")

    class _Page:
        def set_default_navigation_timeout(self, _ms: int) -> None:
            return None

    pw = _FakePlaywright(events)

    class _Chromium:
        async def launch_persistent_context(self, _dir: str, **_kw: Any) -> Any:
            return _Context()

    async def _start() -> Any:
        pw.chromium = _Chromium()  # type: ignore[attr-defined]
        return pw

    async_api = types.ModuleType("playwright.async_api")
    async_api.async_playwright = lambda: types.SimpleNamespace(start=_start)  # type: ignore[attr-defined]
    playwright_pkg = types.ModuleType("playwright")
    playwright_pkg.async_api = async_api  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "playwright", playwright_pkg)
    monkeypatch.setitem(sys.modules, "playwright.async_api", async_api)

    async def _hang_on_launch(profile_dir: Path, headless: bool, chrome_bin: str) -> Any:
        # 浏览器已建好、句柄还没交回调用方时被打断（取消正落在 await 上）
        page = _Page()
        page._x_login_context = _Context()  # type: ignore[attr-defined]
        page._x_login_pw = pw  # type: ignore[attr-defined]
        await asyncio.sleep(10)
        return page

    with tempfile.TemporaryDirectory() as td:
        os.environ["X_PROFILE_DIR"] = str(Path(td) / "p")
        os.environ["X_SESSION_DIR"] = str(Path(td) / "s")
        flow = XLoginFlow(
            config=XLoginConfig.from_config({"login_timeout_sec": 0.3}, headless=True),
            hooks=XLoginHooks(launch=_hang_on_launch, teardown=None),  # 用真 _teardown_browser
        )
        flow.ops = PageOps(
            goto=lambda url, **kw: asyncio.sleep(10),  # 导航挂死 → 触发整链超时
            fill=lambda *a, **kw: asyncio.sleep(0),
            click=lambda *a, **kw: asyncio.sleep(0),
            press=lambda *a, **kw: asyncio.sleep(0),
            wait_for=lambda *a, **kw: asyncio.sleep(0),
            present=lambda *a, **kw: asyncio.sleep(0),
            cookies=lambda: asyncio.sleep(0, result=[]),
            screenshot=lambda *a, **kw: asyncio.sleep(0, result=b""),
        )
        result = _run(flow.login("alice", "pw"))

    assert not result.success and "timed out" in result.message
    assert events == ["context.close", "playwright.stop"], f"超时路径必须真收尾，实际：{events}"


def test_intervention_handler_defaults_raise() -> None:
    handler = NoIntervention()
    for call in (lambda: handler.on_2fa("a"), lambda: handler.on_qrcode("a", "/tmp/x.png")):
        try:
            call()
        except InterventionRequired as exc:
            assert exc.kind in ("two_factor", "qrcode")
        else:
            raise AssertionError("默认 handler 必须拒绝人工介入")


def test_outcome_needs_manual_flag() -> None:
    """needs_manual = 「介入没闭环」，**不等于**「页面要求过介入」。

    intervention 记录页面要求过哪种介入；2FA 码取到并提交成功后它依然是 "two_factor"，
    此时登录成功，needs_manual 必须为 False —— 否则 /login 会返回 ok=true + needs_manual=true，
    board 会当成「仍需人工」反复带码重登（几次就撞风控）。
    """
    assert XLoginOutcome().needs_manual is False
    # 要求 2FA 且码已提交：介入闭环 → 不再需要人工
    assert XLoginOutcome(intervention="two_factor").needs_manual is False
    # 要求介入但没拿到输入 / 没跑完：才叫「停在人工介入上」
    assert XLoginOutcome(intervention="two_factor", manual_outstanding=True).needs_manual is True
    assert XLoginOutcome(intervention="qrcode", qrcode_path="", manual_outstanding=True).needs_manual is True


# —— 11. 浏览器生命周期（Playwright 关闭 API）——


class _FakeContext:
    def __init__(self, events: list[str], fail: bool = False) -> None:
        self.events = events
        self.fail = fail

    async def close(self) -> None:
        self.events.append("context.close")
        if self.fail:
            raise RuntimeError("context close boom")


class _FakePlaywright:
    """async_api.Playwright 只有 stop()、**没有 close()** —— 用「没有 close」真实还原。"""

    def __init__(self, events: list[str], fail: bool = False) -> None:
        self.events = events
        self.fail = fail

    async def stop(self) -> None:
        self.events.append("playwright.stop")
        if self.fail:
            raise RuntimeError("stop boom")


def _page_with_handles(events: list[str], *, fail: bool = False) -> Any:
    page = types.SimpleNamespace()
    page._x_login_context = _FakeContext(events, fail=fail)
    page._x_login_pw = _FakePlaywright(events, fail=fail)
    return page


def test_teardown_stops_playwright_driver_and_closes_context() -> None:
    """teardown 必须 context.close() + playwright.stop()（context 先、driver 后）。

    对 Playwright 调 close() 会抛 AttributeError 被吞成 warning，结果是每次登录都留下
    一个没停掉的 node driver 进程。
    """
    events: list[str] = []
    _run(XLoginFlow(config=XLoginConfig.from_config({}, headless=True))._teardown_browser(
        _page_with_handles(events)
    ))
    assert events == ["context.close", "playwright.stop"]


def test_teardown_survives_close_failures() -> None:
    """收尾任一步失败都不能抛出：teardown 在 finally 里，抛了会盖掉真实登录结果。"""
    events: list[str] = []
    _run(XLoginFlow(config=XLoginConfig.from_config({}, headless=True))._teardown_browser(
        _page_with_handles(events, fail=True)
    ))
    assert events == ["context.close", "playwright.stop"]


def test_teardown_tolerates_missing_handles() -> None:
    """launch 半途失败时 page 上什么都没挂，teardown 必须安静通过。"""
    _run(XLoginFlow(config=XLoginConfig.from_config({}, headless=True))._teardown_browser(
        types.SimpleNamespace()
    ))


def test_launch_stops_driver_when_browser_start_fails(monkeypatch: Any) -> None:
    """起浏览器失败时必须先 pw.stop() 再抛，否则每次失败漏一个 node driver 进程。"""
    events: list[str] = []
    pw = _FakePlaywright(events)

    class _Chromium:
        async def launch_persistent_context(self, *_a: Any, **_kw: Any) -> Any:
            raise RuntimeError("no chromium in sandbox")

    async def _start() -> Any:
        return pw

    async_api = types.ModuleType("playwright.async_api")
    async_api.async_playwright = lambda: types.SimpleNamespace(start=_start)  # type: ignore[attr-defined]
    playwright_pkg = types.ModuleType("playwright")
    playwright_pkg.async_api = async_api  # type: ignore[attr-defined]
    monkeypatch.setitem(sys.modules, "playwright", playwright_pkg)
    monkeypatch.setitem(sys.modules, "playwright.async_api", async_api)

    flow = XLoginFlow(config=XLoginConfig.from_config({}, headless=True))
    try:
        _run(flow._launch_browser(Path("/tmp/no-such-profile"), True, ""))
        raise AssertionError("launch 失败必须抛 RuntimeError")
    except RuntimeError as exc:
        assert "launch chromium for x login failed" in str(exc)
    assert events == ["playwright.stop"]


# —— 12. XAdapter 门面 ——


def test_adapter_login_delegates_with_headless_flag(isolated_roots: dict[str, Any]) -> None:
    page = FakePage(cookies=_login_cookies(), present=LOGIN_FORM | LOGGED_IN)

    async def _launch(profile_dir: Path, headless: bool, chrome_bin: str) -> FakePage:
        page.actions.append(("launch", f"headless={headless}"))
        return page

    async def _teardown(_p: Any) -> None:
        page.actions.append(("teardown", ""))

    adapter = XAdapter(
        config={"headless": True, "base_url": "https://x.com"},
        hooks=XLoginHooks(launch=_launch, teardown=_teardown),
    )
    adapter.login_flow.ops = _ops(page)
    adapter.login_flow.hooks = XLoginHooks(launch=_launch, teardown=_teardown)

    result = _run(adapter.login("alice", "pw", headless=False))
    assert result.success
    launches = [a[1] for a in page.actions if a[0] == "launch"]
    assert launches and "headless=False" in launches[0], "login(headless=...) 必须透传到浏览器"

    # 登录态读写门面（执行链复用登录态，不必重登）
    got = adapter.session("alice")
    assert got is not None and got.valid
    assert Path(adapter.profile_dir("alice")) == profile_dir_for("alice")
    assert Path(adapter.session_file("alice")) == session_file_for("alice")


class _MiniMonkeyPatch:
    """直跑模式下的最小 monkeypatch（``setitem`` / ``setenv`` / ``delenv`` + 统一还原）。

    只为「无 pytest 直跑」这一条路径服务：让同一份用例文件在 pytest 与直跑两种模式下
    断言完全一致，不因为缺少 fixture 就跳过用例。
    """

    _MISSING = object()

    def __init__(self) -> None:
        self._items: list[tuple[Any, Any, Any]] = []  # (mapping, key, 原值或 _MISSING)
        self._envs: list[tuple[str, Any]] = []  # (name, 原值或 None)

    def setitem(self, mapping: Any, key: Any, value: Any) -> None:
        self._items.append((mapping, key, mapping.get(key, self._MISSING)))
        mapping[key] = value

    def setenv(self, name: str, value: str) -> None:
        self._envs.append((name, os.environ.get(name)))
        os.environ[name] = value

    def delenv(self, name: str) -> None:
        self._envs.append((name, os.environ.get(name)))
        os.environ.pop(name, None)

    def undo(self) -> None:
        for mapping, key, old in reversed(self._items):
            if old is self._MISSING:
                mapping.pop(key, None)
            else:
                mapping[key] = old
        self._items.clear()
        for name, old in reversed(self._envs):
            if old is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = old
        self._envs.clear()


def _main() -> None:
    """无 pytest 时的直跑契约：按顺序跑完所有单测（fixture 手工传入）。

    需要 ``monkeypatch`` 的用例由直跑 runner 注入一个最小实现（``setitem`` / ``setenv``
    / ``delenv``，在用例结束后统一还原），因此同一份用例文件在 pytest 与直跑两种模式下
    断言完全一致。
    """
    import inspect
    import tempfile

    failures = 0
    for name, fn in sorted(globals().items()):
        if not (name.startswith("test_") and callable(fn)):
            continue
        params = inspect.signature(fn).parameters
        kwargs: dict[str, Any] = {}
        with tempfile.TemporaryDirectory() as td:
            if "isolated_roots" in params:
                kwargs["isolated_roots"] = isolated_roots(Path(td))
            patch = _MiniMonkeyPatch() if "monkeypatch" in params else None
            if patch is not None:
                kwargs["monkeypatch"] = patch
            try:
                fn(**kwargs)
                print(f"✅ {name}")
            except Exception as exc:  # noqa: BLE001
                failures += 1
                print(f"❌ {name}: {type(exc).__name__}: {exc}")
            finally:
                if patch is not None:
                    patch.undo()
    if failures:
        raise SystemExit(f"{failures} test(s) failed")
    print("✅ all pyreplay x_login checks passed")


if __name__ == "__main__":
    _main()
