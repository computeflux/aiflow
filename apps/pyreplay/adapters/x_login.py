"""X (Twitter) 登录会话持久化（M1-3，工作项 wi-1795586435-e694）。

把散落在 pycraper/pyreplay 里的「X 登录态 = auth_token + ct0 cookie」这条约定，
补齐成一条**真正的登录链**：账号密码登录 → 需要时走 2FA → 从浏览器读出
auth_token / ct0 → 按账号隔离落盘（profile 目录 + session.json）→ 回报 AccountManager。

设计要点：

* **不依赖 crawl4ai**：登录是长会话交互（输密码、等 2FA、等跳转），crawl4ai 的
  「一次性抓取 + hook」模型套不进去。这里直接用 crawl4ai 已经装好的
  ``playwright.async_api``（pyreplay 的 .venv 里就有，见 uv.lock），不新增依赖。
* **按账号隔离**：每个账号一份独立的持久化 profile 目录 + session 文件，路径由
  :func:`profile_dir_for` / :func:`session_file_for` 决定；根目录取
  ``X_PROFILE_DIR`` / ``X_SESSION_DIR``（默认 ``<pyreplay>/data/x``）。目录名做安全化，
  绝不能逃出根目录。
* **人工介入是接口，不是 TODO**：:class:`ManualInterventionHandler` 定义
  ``on_2fa`` / ``on_qrcode``，默认实现 :class:`NoIntervention` 直接抛
  :class:`InterventionRequired`，把「要人介入」这个事实原样回报给调用方（登录结果里
  ``needs_manual=True`` + 二维码落盘路径），不阻塞、不猜。
* **回报 AccountManager**：登录成功写 ``x_accounts``（按 machine_id + username 幂等，
  ``last_login_at`` / ``last_login_error`` 记入 profile）；MySQL 未就绪时降级为只落盘，
  登录结果里 ``account_manager="skipped: ..."``，主链路不受影响。

红线：auth_token / ct0 是登录态明文。本模块只在内存与 **0600** 权限的账号 session
文件之间流转；日志与结果摘要里一律脱敏（:func:`mask_secret`）。
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
import re
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from pydantic import BaseModel, Field

logger = logging.getLogger(__name__)

# X 的登录入口与登录后落地页
X_LOGIN_URL = "https://x.com/i/flow/login"
X_HOME_URL = "https://x.com/home"

# X 登录页元素（2026-11 版 flow/login；input 用 autocomplete 定位，比 name/placeholder 稳）。
# 每步表单用 press Enter 提交：X 的「Next / Log in」本质是表单提交，比点 [role='button'] 稳
# （那个按钮常带 aria-disabled，且文案随界面语言变化）。
_X_USERNAME_INPUT = (
    "input[autocomplete='username'], input[name='username'], "
    "input[aria-label='Phone, email, or username']"
)
_X_PASSWORD_INPUT = "input[name='password'], input[autocomplete='current-password'], input[aria-label='Password']"
_X_OAUTH_VERIFICATION_INPUT = (
    "input[name='text-code'], input[aria-label='Verification code'], "
    "input[autocomplete='one-time-code'], input[data-testid='ocfEnterTextTextInput']"
)
# 扫码登录的二维码载体：X 用 canvas 画码（个别版本是 data-uri img）
_X_QRCODE_IMG = "canvas, img[src^='data:image'], [data-testid='loginSecurityKeyQrCode'] img"
_X_QRCODE_CANVAS = "canvas"
# 读 cookies 的范围（Playwright context.cookies 的 url 参数）
X_COOKIE_URL = "https://x.com"

# 判定「已登录」的元素（登录后首页才会出现）
_X_LOGGED_IN_SELECTORS = [
    '[data-testid="AppTabBar_Profile-Link"]',
    '[data-testid="SideNav_AccountSwitcher_Button"]',
    'a[href="/home"]',
]

# X 会话 cookie：auth_token = 登录态本体，ct0 = CSRF token（两者缺一不可）
X_SESSION_COOKIE_NAMES = ("auth_token", "ct0", "username")

# 安全化账号标识：只留字母数字与 _ -，其余（含 `.` `/` 空格）压成 _。
# **不**放行点号：`..` / `../../etc/passwd` 这类输入必须整体压平，绝不能留下可穿越的路径段。
_SAFE_ACCOUNT_RE = re.compile(r"[^A-Za-z0-9_-]+")


def sanitize_account_id(account: str) -> str:
    """把账号标识压成**安全的单层目录名**（防路径穿越：``../../etc`` → ``_etc``）。"""
    return _SAFE_ACCOUNT_RE.sub("_", (account or "").strip()) or "_"


def profile_root() -> Path:
    """持久化 profile 根目录：``X_PROFILE_DIR``，默认 ``<pyreplay>/data/x/profiles``。"""
    return _resolve_root(os.environ.get("X_PROFILE_DIR"), "profiles")


def session_root() -> Path:
    """登录态 session 根目录：``X_SESSION_DIR``，默认 ``<pyreplay>/data/x/sessions``。"""
    return _resolve_root(os.environ.get("X_SESSION_DIR"), "sessions")


def _resolve_root(env_value: str | None, subdir: str) -> Path:
    """解析持久化根目录。

    ⚠️ 环境变量给的是**根目录本身**，不再追加 profiles/sessions 子目录 —— 否则
    测试与运维把 X_PROFILE_DIR/X_SESSION_DIR 指到同一目录时，两者的账号材料会
    叠在 `.../profiles/profiles`、`.../sessions/sessions` 这种嵌套层里（路径与
    部署预期不一致，排障时极难看出）。
    """
    base = (env_value or "").strip()
    if base:
        return Path(base).expanduser()
    return Path(__file__).resolve().parent.parent / "data" / "x" / subdir


def profile_dir_for(account: str, root: Path | None = None) -> Path:
    """账号专属的浏览器 profile 目录（Chromium user-data-dir，按账号隔离）。"""
    return (root or profile_root()) / sanitize_account_id(account)


def session_file_for(account: str, root: Path | None = None) -> Path:
    """账号专属的登录态文件（0600，落 auth_token/ct0/username）。"""
    return (root or session_root()) / f"{sanitize_account_id(account)}.json"


def mask_secret(value: str, keep: int = 4) -> str:
    """登录态脱敏：只保留首尾各 ``keep`` 个字符（短串整体打码）。"""
    v = value or ""
    if not v:
        return ""
    if len(v) <= keep * 2:
        return "*" * len(v)
    return f"{v[:keep]}…{v[-keep:]}({len(v)})"


class XSession(BaseModel):
    """一个账号的 X 登录态（auth_token + ct0 + 展示用 username）。"""

    auth_token: str = ""
    ct0: str = ""
    username: str = ""

    @property
    def valid(self) -> bool:
        """auth_token 与 ct0 齐备才算可用登录态。"""
        return bool(self.auth_token and self.ct0)

    def cookies(self) -> list[dict[str, Any]]:
        """转成 Playwright / crawl4ai 的 cookie dict 列表（.x.com 域）。"""
        out: list[dict[str, Any]] = []
        if self.auth_token:
            out.append({
                "name": "auth_token",
                "value": self.auth_token,
                "domain": ".x.com",
                "path": "/",
                "secure": True,
                "httpOnly": True,
                "sameSite": "None",
            })
        if self.ct0:
            out.append({
                "name": "ct0",
                "value": self.ct0,
                "domain": ".x.com",
                "path": "/",
                "secure": True,
                "httpOnly": False,
                "sameSite": "None",
            })
        return out


def load_session(account: str, root: Path | None = None) -> XSession | None:
    """读账号已持久化的登录态；文件缺失/损坏返回 None（不抛，登录链自行决定重登）。"""
    path = session_file_for(account, root)
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None
    except (json.JSONDecodeError, OSError) as exc:
        logger.warning("session file %s unreadable: %s", path, exc)
        return None
    if not isinstance(raw, dict):
        logger.warning("session file %s is not a JSON object", path)
        return None
    return XSession(
        auth_token=str(raw.get("auth_token") or ""),
        ct0=str(raw.get("ct0") or ""),
        username=str(raw.get("username") or ""),
    )


def save_session(account: str, session: XSession, root: Path | None = None) -> Path:
    """写账号登录态文件（父目录 0700 / 文件 **0600**，登录态是敏感数据）。"""
    path = session_file_for(account, root)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    payload = {
        "account": sanitize_account_id(account),
        "username": session.username,
        "auth_token": session.auth_token,
        "ct0": session.ct0,
        "saved_at": int(time.time()),
    }
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
    os.chmod(path, 0o600)
    return path


class LoginResult(BaseModel):
    """一次登录的结果（回报 AccountManager 的载荷）。"""

    account: str
    success: bool
    username: str = ""
    auth_token: str = ""
    ct0: str = ""
    # 人工介入（2FA / 扫码）：needs_manual=True 时 success 必为 False
    needs_manual: bool = False
    intervention: str = ""  # two_factor | qrcode | ""
    qrcode_path: str = ""  # 扫码介入时二维码落盘路径
    message: str = ""
    session_file: str = ""
    profile_dir: str = ""
    # 会话来源：password（账密登录）/ reuse（复用已持久化登录态）/ manual（人工介入后取得）
    source: str = ""
    account_manager: str = ""  # 回报结果：updated | created | skipped: <原因> | failed: <原因>
    at: float = Field(default_factory=time.time)

    def summary(self) -> dict[str, Any]:
        """脱敏摘要（日志 / API 响应可用，绝不含登录态明文）。"""
        return {
            "account": self.account,
            "success": self.success,
            "username": self.username,
            "needs_manual": self.needs_manual,
            "intervention": self.intervention,
            "qrcode_path": self.qrcode_path,
            "message": self.message,
            "session_file": self.session_file,
            "profile_dir": self.profile_dir,
            "source": self.source,
            "account_manager": self.account_manager,
            "auth_token": mask_secret(self.auth_token),
            "ct0": mask_secret(self.ct0),
        }


class InterventionRequired(RuntimeError):
    """人工介入被拒绝（默认 handler 的行为）：登录链把它转成 needs_manual 结果。"""

    def __init__(self, kind: str, message: str = "") -> None:
        super().__init__(message or f"manual intervention required: {kind}")
        self.kind = kind


class ManualInterventionHandler:
    """2FA / 扫码登录的人工介入接口（占位实现，供后续接 Matrix/board 审批）。

    子类只需覆盖用到的方法；基类行为 = **拒绝介入**（抛 :class:`InterventionRequired`），
    这样调用方拿到的是明确的「需要人工」而不是超时挂死。

    ``on_2fa`` 返回一次性验证码（str）；``on_qrcode`` 返回 None 表示「人已扫完」。
    """

    def on_2fa(self, account: str, hint: str = "") -> str | None:
        raise InterventionRequired("two_factor", f"account {account!r} requires a 2FA code (no handler installed)")

    def on_qrcode(self, account: str, qrcode_path: str = "") -> None:
        raise InterventionRequired("qrcode", f"account {account!r} requires QR-code login (no handler installed)")


class CallbackIntervention(ManualInterventionHandler):
    """把介入回调接成 handler：``on_2fa(account, hint) -> code``、``on_qrcode(account, path) -> None``。

    传 None 的回调保持基类的「拒绝介入」语义（便于只接 2FA、不接扫码）。
    """

    def __init__(
        self,
        two_factor: Callable[[str, str], str | None] | None = None,
        qrcode: Callable[[str, str], None] | None = None,
    ) -> None:
        self._two_factor = two_factor
        self._qrcode = qrcode

    def on_2fa(self, account: str, hint: str = "") -> str | None:
        if self._two_factor is None:
            return super().on_2fa(account, hint)
        return self._two_factor(account, hint)

    def on_qrcode(self, account: str, qrcode_path: str = "") -> None:
        if self._qrcode is None:
            return super().on_qrcode(account, qrcode_path)
        self._qrcode(account, qrcode_path)


class NoIntervention(ManualInterventionHandler):
    """默认 handler：任何人工介入都直接拒绝（headless 无人值守场景）。"""


@dataclass
class XLoginConfig:
    """XAdapter.login 的运行参数（来自 config/x.json，缺省即安全默认）。"""

    headless: bool = True
    chrome_bin: str = ""
    base_url: str = "https://x.com"
    # 单步动作（导航/等元素）超时；登录整链超时 = login_timeout
    action_timeout_ms: int = 30_000
    login_timeout: float = 300.0
    # 2FA 验证码长度上限（X 的 TOTP 是 6 位；只做形状防御，不做业务校验）
    two_factor_code_max_len: int = 16
    # 探测「页面要 2FA 还是要扫码」的有界超时（未命中即返回，绝不长等）
    intervention_probe_timeout_ms: int = 1500
    # 登录成功后顺手把会话写盘 + 回报 AccountManager
    persist_session: bool = True
    notify_account_manager: bool = True

    @classmethod
    def from_config(cls, config: dict[str, Any], headless: bool | None = None) -> XLoginConfig:
        cfg = cls()
        cfg.headless = bool(config.get("headless", True)) if headless is None else headless
        cfg.base_url = str(config.get("base_url") or "https://x.com").rstrip("/")
        try:
            cfg.action_timeout_ms = int(config.get("login_action_timeout_ms") or cfg.action_timeout_ms)
        except (TypeError, ValueError):
            pass
        try:
            cfg.login_timeout = float(config.get("login_timeout_sec") or cfg.login_timeout)
        except (TypeError, ValueError):
            pass
        try:
            cfg.two_factor_code_max_len = int(
                config.get("login_2fa_code_max_len") or cfg.two_factor_code_max_len
            )
        except (TypeError, ValueError):
            pass
        try:
            cfg.intervention_probe_timeout_ms = int(
                config.get("login_intervention_probe_ms") or cfg.intervention_probe_timeout_ms
            )
        except (TypeError, ValueError):
            pass
        cfg.persist_session = bool(config.get("login_persist_session", True))
        cfg.notify_account_manager = bool(config.get("login_notify_account_manager", True))
        return cfg


@dataclass
class XLoginHooks:
    """登录链的浏览器接缝（单测注入点；生产用默认值 = 真 Playwright）。

    ``launch``: async (profile_dir, headless, chrome_bin) → page（已导航到登录页）
    ``teardown``: async (page) → None（launch 之后必然调用，异常也调）
    """

    launch: Callable[..., Any] | None = None
    teardown: Callable[..., Any] | None = None
    # 人工介入接缝：None = 走 handler（默认拒绝）；非 None = 直接取介入输入（单测用）
    two_factor_provider: Callable[[str, str], str | None] | None = None
    qrcode_provider: Callable[[str, str], None] | None = None


@dataclass
class PageOps:
    """登录链对页面用到的**动作接缝**（动词签名 = Playwright Page 的签名）。

    默认实现 :meth:`PlaywrightPageOps.for_timeout` 直接转发给真 Page（fill/click/
    press/cookies/screenshot，参数与 Playwright 完全一致）；单测注入 fake 时签名
    不变，所以被测的就是**生产路径本身**，不存在「测试适配层」与真实浏览器行为分叉。
    """

    goto: Callable[..., Any]
    fill: Callable[..., Any]
    click: Callable[..., Any]
    press: Callable[..., Any]
    wait_for: Callable[..., Any]
    present: Callable[..., Any]
    cookies: Callable[..., Any]
    screenshot: Callable[..., Any]

    @classmethod
    def for_timeout(cls, page: Any, timeout_ms: int) -> PageOps:
        """把真 Playwright Page 包成动词接缝（单动作超时统一注入）。"""
        return cls(
            goto=lambda url, **kw: page.goto(url, timeout=timeout_ms, **kw),
            fill=lambda selector, value, **kw: page.fill(selector, value, timeout=timeout_ms, **kw),
            click=lambda selector, **kw: page.click(selector, timeout=timeout_ms, **kw),
            press=lambda selector, key, **kw: page.press(selector, key, timeout=timeout_ms, **kw),
            wait_for=lambda selector, **kw: page.wait_for_selector(selector, timeout=timeout_ms, **kw),
            present=lambda selector, ms: cls._present(page, selector, ms),
            cookies=lambda: page.context.cookies(X_COOKIE_URL),
            screenshot=lambda selector=None: cls._screenshot(page, selector),
        )

    @staticmethod
    async def _present(page: Any, selector: str, timeout_ms: int) -> bool:
        """有界探测「元素是否出现」（扫码 / 2FA 分支判定）。

        ⚠️ 探测必须**有界**：Playwright 的 wait_for_selector 不传 timeout 时用的是
        context 默认超时（30s），元素不出现就白等 30 秒。命中判定只看**有没有抛异常**：
        返回值在 Playwright 是 Element、在测试替身里是 None，拿返回值做真值判定会把
        「元素已出现」误判成「不存在」。
        """
        try:
            await page.wait_for_selector(selector, timeout=timeout_ms)
        except Exception:  # noqa: BLE001 - 超时/未渲染 = 元素不存在
            return False
        return True

    @staticmethod
    async def _screenshot(page: Any, selector: str | None = None) -> bytes:
        """元素级截图（二维码 canvas/img）；取不到元素时退整页截图。"""
        if selector:
            try:
                return bytes(await page.locator(selector).first.screenshot())
            except Exception as exc:  # noqa: BLE001 - 元素定位失败（改版/未渲染）走整页
                logger.debug("element screenshot failed (%s): %s", selector, exc)
        return bytes(await page.screenshot())


@dataclass
class XLoginOutcome:
    """登录链内部结果（page 层）：会话 + 人工介入请求 + 失败原因。"""

    session: XSession | None = None
    intervention: str = ""
    qrcode_path: str = ""
    message: str = ""
    # 会话来源：password（账密）/ manual（2FA 或扫码介入后取得）
    source: str = ""
    # 介入**没闭环**（需要人做还没做完的事）：占位 handler 拒绝、扫码码取不到等。
    # 与 intervention（页面**要求**哪种介入）分开：要求 2FA 且码已提交成功时
    # intervention="two_factor" 但 manual_outstanding=False，登录照样算成功。
    manual_outstanding: bool = False

    @property
    def needs_manual(self) -> bool:
        """登录链**停在人工介入上**（介入没闭环）：等价于 manual_outstanding。

        刻意**不看** intervention：那个字段记录「页面要求过哪种介入」，介入做完它依然有值；
        用它派生 needs_manual 会把成功的 2FA 登录标成「仍需人工」。留这个属性是为了让
        「介入未完成」在读代码时一眼可辨，写断言时也不会把两个概念混用一个字段。
        """
        return self.manual_outstanding


def _in_cancellation() -> bool:
    """当前协程是否已被要求取消（``asyncio.current_task().cancelling()``，py3.11+）。"""
    try:
        task = asyncio.current_task()
    except RuntimeError:  # 无运行中的 loop
        return False
    return task is not None and task.cancelling() > 0


def _run_cleanup(coro_factory: Callable[[], Any], *, what: str, timeout: float = 30.0) -> None:
    """**取消安全**的收尾执行器：登记收尾任务给驱动层补跑，本协程随即结束自己。

    为什么必须这么绕：``asyncio.wait_for`` 的超时是**取消**内层协程实现的，而一个已被取消的
    协程里 ``await`` 任何未就绪的对象都会立刻再抛 CancelledError —— 连 ``asyncio.shield``
    也一样（shield 只让内层任务在外层取消时继续存活，**不会**让外层 await 多等一秒）。
    结果就是 ``finally`` 里的收尾一行都没执行：persistent context + node driver 进程泄漏，
    账号的 user-data-dir 还留着 SingletonLock，下次登录同一账号直接起不来。

    ⚠️ 两条看起来更简单的路都走不通（都实测过）：
    - 「后台线程 ``loop.run_until_complete(收尾)`` + 当前线程 ``thread.join()``」：**必死锁**。
      当前循环正阻塞在 ``wait_for`` 里等内层任务结束，而内层协程的 ``finally`` 就是这次调用，
      它同步阻塞在 join 上，那个循环一步都推不动。
    - 「当前协程忙轮询一个 ``loop.create_task`` 出来的兄弟任务」：**推不动它**。事件循环只在
      控制权真正交还时才推进别的任务；带「待取消」标记的协程里 await 立刻抛，控制流从没离开
      当前调用栈，兄弟任务永远排不上（连 ``time.sleep`` 都帮不上 —— 循环被我们占着）。

    可行写法：收尾任务交给**驱动登录链的那一层**去跑 —— 本协程只把它登记到当前任务上就立刻
    结束自己；``login()`` 在 ``wait_for`` 返回后（那一层不带取消标记，能正常 await）通过
    :func:`_drain_cleanups` 把它们逐个跑完。
    """
    # 协程对象没有 __dict__、挂不了属性；收尾清单挂在**当前任务**上（Task 可写属性），
    # 由登录链的驱动层（login）在 wait_for 之后取回并跑完。
    owner = asyncio.current_task()
    if owner is None:
        raise RuntimeError("_run_cleanup must run inside a task")
    box = getattr(owner, "_x_login_cleanups", None)
    if box is None:
        box = []
        setattr(owner, "_x_login_cleanups", box)
    # 取消上下文里 create_task 出的任务不会被本协程的取消连带取消，登记后交给驱动层跑完
    box.append((what, asyncio.create_task(coro_factory()), timeout))


async def _drain_cleanups(owner: asyncio.Task | None) -> None:
    """跑完挂在当前任务上的收尾任务（登录链的驱动层在超时/取消后调用）。"""
    box = getattr(owner, "_x_login_cleanups", None) if owner is not None else None
    if not box:
        return
    for what, task, timeout in list(box):
        try:
            await asyncio.wait_for(task, timeout=timeout)
        except asyncio.CancelledError:
            task.cancel()
            logger.warning("x login %s cancelled while draining cleanups", what)
        except Exception as exc:  # noqa: BLE001 - 收尾失败只记日志，绝不冒泡
            logger.warning("x login %s failed: %s", what, exc)
    box.clear()


class XLoginFlow:
    """X 登录流程执行器（页面动作全部经 :class:`PageOps` 动词接缝，可离线单测）。

    真实登录需要真浏览器；本类的**编排逻辑**（先复用登录态、缺密码就要求人工、
    2FA / 扫码走介入接口、成功后落盘 + 回报 AccountManager）与浏览器无关：
    单测注入签名一致的 fake PageOps，跑的就是生产路径本身。
    """

    def __init__(
        self,
        config: XLoginConfig | None = None,
        intervention: ManualInterventionHandler | None = None,
        hooks: XLoginHooks | None = None,
        ops: PageOps | None = None,
        account_manager: Any | None = None,
    ) -> None:
        self.config = config or XLoginConfig()
        self.intervention = intervention or NoIntervention()
        self.hooks = hooks or XLoginHooks()
        self.ops = ops
        self.account_manager = account_manager
        # 当前浏览器会话（launch 之后、teardown 之前有值）：外部取消路径的收尾凭据
        self._active_page: Any = None
        # (account, profile_dir)：page 尚未建出来就被取消时，按账号目录释放 persistent profile
        self._session: tuple[str, Path] | None = None

    # —— 主入口 ——

    async def login(
        self,
        account: str,
        password: str = "",
        *,
        headless: bool | None = None,
        force: bool = False,
        two_factor_code: str = "",
    ) -> LoginResult:
        """登录一个账号并把登录态按账号隔离持久化。

        Args:
            account: 账号标识（用户名 / 邮箱 / x_accounts.username）。用于隔离 profile，
                同时是未显式给用户名时的登录名。
            password: 密码。为空且已持久化登录态时优先复用登录态（``source="reuse"``）。
            headless: 覆盖配置的无头开关（headed 用于人工扫码/看页面）。
            force: True = 即使已有有效登录态也重走登录（换号/登录态被吊销时用）。
            two_factor_code: 已知的 2FA 码；为空且页面要求 2FA 时问 intervention handler。
        """
        if not (account or "").strip():
            return LoginResult(account=account, success=False, message="account is required")

        cfg = self.config
        if headless is not None:
            cfg.headless = headless

        profile_dir = profile_dir_for(account)
        # 登录整链超时（wall clock）：浏览器导航/等元素各有单步超时，但介入 handler、
        # 重试循环、外部实现都可能把一次登录拖成无限期 —— 登录跑在 skill 服务线程里，
        # 挂死就等于占死这个账号的登录通道。超时统一落到 LoginResult，不冒泡打崩调用方。
        deadline = time.monotonic() + max(1.0, float(self.config.login_timeout))

        def _expired() -> LoginResult:
            logger.error("x login timed out for %s after %ss", account, self.config.login_timeout)
            return LoginResult(
                account=account,
                success=False,
                profile_dir=str(profile_dir),
                session_file=str(session_file_for(account)),
                message=f"login timed out after {self.config.login_timeout:g}s",
            )

        result = LoginResult(
            account=account,
            success=False,
            profile_dir=str(profile_dir),
            session_file=str(session_file_for(account)),
        )

        # 1) 复用：已持久化的登录态可用且未 force → 直接回报，不起浏览器
        existing = load_session(account)
        if not force and existing is not None and existing.valid and not password:
            result.success = True
            result.source = "reuse"
            result.username = existing.username
            result.auth_token = existing.auth_token
            result.ct0 = existing.ct0
            result.message = "reused persisted session"
            result.account_manager = self._report(result)
            return result

        if time.monotonic() > deadline:
            return _expired()

        # 2) 账密登录的前置校验：缺密码时不打开浏览器（headless 下必然卡死在密码框）
        if not password:
            result.needs_manual = True
            result.intervention = "password"
            result.message = "no password provided and no persisted session for this account"
            result.account_manager = self._report(result)
            return result

        if time.monotonic() > deadline:
            return _expired()

        # 3) 跑登录流程
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return _expired()

        # ⚠️ 必须**显式**建 Task 并持有它：asyncio.wait_for(coro, ...) 内部会 ensure_future(coro)
        # 新建一个 Task，登录协程里的 _run_cleanup 登记收尾时拿到的 current_task() 就是它；
        # 如果这里只在超时分支用 current_task() 去取清单，取到的是**外层 login() 的 Task**，
        # 清单永远为空 —— 登记-补跑通道静默空转，收尾任务只能在 asyncio.run() 退出时被
        # _cancel_all_tasks 直接 cancel 掉（真浏览器的 context.close()/stop() 必然有挂起点）。
        login_task = asyncio.ensure_future(self._run_login(account, password, two_factor_code, cfg))
        try:
            # 整链超时：launch + 全部页面动作 + 介入都在保护范围内。
            outcome = await asyncio.wait_for(login_task, timeout=remaining)
        except InterventionRequired as exc:
            outcome = XLoginOutcome(
                intervention=getattr(exc, "kind", "manual"),
                qrcode_path=getattr(exc, "qrcode_path", "") or "",
                message=str(exc),
                manual_outstanding=True,
            )
        except (TimeoutError, asyncio.CancelledError):
            # asyncio.wait_for 超时（py3.12: TimeoutError 即 asyncio.TimeoutError）。
            # 浏览器会话已在 _run_login 的取消路径里就地收口（_release_session），
            # 这里兜一层幂等收尾，再落一个超时结果。
            # 兜一层幂等收尾（正常取消路径已就地收口），再把登录 Task 上登记的收尾任务跑完。
            await self._release_session()
            await _drain_cleanups(login_task)
            return _expired()
        except Exception as exc:  # noqa: BLE001 - 登录链任何异常都要变成结果，不能冒泡打崩调用方
            logger.error("x login failed for %s: %s", account, exc)
            outcome = XLoginOutcome(message=str(exc) or type(exc).__name__)

        result.intervention = outcome.intervention
        result.qrcode_path = outcome.qrcode_path
        result.message = outcome.message
        # needs_manual 只表示「介入**没闭环**」。intervention 表示「页面要求过哪种介入」，
        # 2FA 码提交成功时它是 "two_factor" 而介入已经完成 —— 若用它派生 needs_manual，
        # 成功登录会带出 needs_manual=True（违反 LoginResult 的约定），board 会误判成
        # 「仍需人工」而反复带码重登，几次就撞风控。
        result.needs_manual = outcome.manual_outstanding

        session = outcome.session
        if session is None or not session.valid:
            if not result.message:
                result.message = "login did not yield a valid session (auth_token/ct0 missing)"
            result.account_manager = self._report(result)
            return result

        # 4) 按账号隔离落盘（profile 目录由浏览器写，这里只写 session 文件）
        result.success = True
        result.source = outcome.source or "password"
        result.username = session.username or account
        result.auth_token = session.auth_token
        result.ct0 = session.ct0
        if cfg.persist_session:
            try:
                result.session_file = str(save_session(account, session))
            except OSError as exc:
                logger.warning("persist session for %s failed: %s", account, exc)
                result.message = f"session captured but persist failed: {exc}"
        else:
            result.session_file = ""

        # 5) 回报 AccountManager（登录成功）
        result.account_manager = self._report(result)
        return result

    # —— 浏览器登录流程 ——

    async def _run_login(
        self, account: str, password: str, two_factor_code: str, cfg: XLoginConfig
    ) -> XLoginOutcome:
        # ⚠️ launch 也在超时保护范围内：起浏览器（persistent context + 导航）是登录链里
        # 最容易挂死的一步，放在 wait_for 之外等于没有保护。
        profile_dir = profile_dir_for(account)
        self._session = (account, profile_dir)
        self._active_page = None
        page: Any = None  # 取消路径要在「赋值语句本身被打断」时也能拿到句柄（那时它是 None）
        try:
            page = await self._launch(account, cfg)
        except asyncio.CancelledError:
            # 起浏览器（persistent context + 导航）是登录链里最容易挂死、也最容易被整链超时
            # 取消打断的一步。此时页面收尾的 finally 还没开始，会话必须就地收口。
            #
            # 句柄由 _launch 自己登记（见其注释）；这里只负责收口。
            await self._release_session()
            raise
        try:
            ops = self._ops_for(page, cfg)
            await ops.goto(f"{cfg.base_url}/i/flow/login")

            # 第一步：用户名（X 的 flow/login 是两步表单；每步用 press Enter 提交，
            # 比点 [role='button'] 稳 —— 那个按钮常带 aria-disabled 且文案随语言变化）
            await ops.wait_for(_X_USERNAME_INPUT)
            await ops.fill(_X_USERNAME_INPUT, account)
            await ops.press(_X_USERNAME_INPUT, "Enter")

            # 第二步：密码 → 提交
            await ops.wait_for(_X_PASSWORD_INPUT)
            await ops.fill(_X_PASSWORD_INPUT, password)
            await ops.press(_X_PASSWORD_INPUT, "Enter")

            # 人工介入：调用方已给 2FA 码时先过形状校验（不合法就要求重新介入，绝不把
            # 垃圾串灌进验证码框按 Enter 提交给 X）；否则探测页面要哪种介入并向 handler
            # 取输入。介入**必须闭环**：占位 handler 抛 InterventionRequired（→ needs_manual），
            # 真 handler 返回了码就提交；扫码同理 —— 码已落盘、handler 拒绝 → 这次没登进去。
            code = (two_factor_code or "").strip()
            via_2fa = bool(code)
            qrcode_path = ""
            manual_outstanding = ""
            if via_2fa:
                self._validate_two_factor_code(code)
            else:
                code, qrcode_path, manual_outstanding = await self._resolve_intervention(
                    account, ops, cfg.intervention_probe_timeout_ms
                )
                via_2fa = bool(code)

            if code:
                await ops.fill(_X_OAUTH_VERIFICATION_INPUT, code)
                await ops.press(_X_OAUTH_VERIFICATION_INPUT, "Enter")

            # 等登录落地：出现首页元素 = 过了登录墙（X 登录后跳 /home）
            landed = False
            for selector in _X_LOGGED_IN_SELECTORS:
                try:
                    await ops.wait_for(selector)
                    landed = True
                    break
                except Exception:  # noqa: BLE001 - 逐个选择器兜底，全部落空才算未登录
                    continue

            cookies = {str(c["name"]): str(c.get("value", "")) for c in await ops.cookies()}
            session = XSession(
                auth_token=cookies.get("auth_token", ""),
                ct0=cookies.get("ct0", ""),
                username=cookies.get("username", ""),
            )
            source = "manual" if (via_2fa or qrcode_path) else "password"
            intervention = "qrcode" if qrcode_path else ("two_factor" if via_2fa else "")

            if manual_outstanding:
                # 介入要求的种类在扫码路径上可能派生不出来（码都没拍到），
                # 兜底成 qrcode：调用方要看的是「要哪种人工介入」，不是内部派生量。
                return XLoginOutcome(
                    intervention=intervention or "qrcode",
                    qrcode_path=qrcode_path,
                    message=manual_outstanding,
                    manual_outstanding=True,
                )

            if not landed:
                return XLoginOutcome(
                    intervention=intervention,
                    qrcode_path=qrcode_path,
                    message="login wall not passed: no logged-in element appeared",
                )
            if not session.valid:
                return XLoginOutcome(
                    intervention=intervention,
                    qrcode_path=qrcode_path,
                    message="logged in but auth_token/ct0 cookies missing (X risk control?)",
                )
            return XLoginOutcome(
                session=session, intervention=intervention, qrcode_path=qrcode_path, source=source
            )
        finally:
            await self._teardown(page)

    def _ops_for(self, page: Any, cfg: XLoginConfig) -> PageOps:
        """页面动作接缝：显式注入优先（单测），否则包真 Playwright Page。"""
        if self.ops is not None:
            return self.ops
        return PageOps.for_timeout(page, cfg.action_timeout_ms)

    async def _launch(self, account: str, cfg: XLoginConfig) -> Any:
        """起浏览器会话并把句柄登记到会话上（取消路径的收尾凭据）。

        ⚠️ 句柄必须在 launch **返回的那一刻**就挂到 self 上，不能等调用方赋值：整链超时
        是取消实现的，``page = await self._launch(...)`` 这条赋值语句本身会被打断，调用方
        接手时 page 永远是 None —— 收尾就会退化成「按目录重开一次 driver 关掉」（对真浏览器
        是白起一个进程去抢已被占用的 profile，对注入的 fake 启动器等于一次都没收尾）。
        """
        profile_dir = profile_dir_for(account)
        self._active_page = None
        page: Any = None
        try:
            if self.hooks.launch is not None:
                page = await self.hooks.launch(profile_dir, cfg.headless, cfg.chrome_bin)
            else:
                page = await self._launch_browser(profile_dir, cfg.headless, cfg.chrome_bin)
        except asyncio.CancelledError:
            # 取消正好落在「对象已建好、赋值未完成」的窗口里：句柄不能丢。
            # 注意这里**不能** await 任何东西（本协程正带「待取消」标记，await 立刻再抛），
            # 所以只登记句柄；真正的收尾由调用方 _run_login 的取消分支负责。
            self._active_page = page
            raise
        self._active_page = page
        return page

    async def _release_session(self) -> None:
        """释放当前浏览器会话：有 page 走页面收尾；page 没建出来才按账号目录释放 profile。

        幂等：句柄取不到（已收过）就直接返回，允许多条路径都调它。
        """
        # ⚠️ 必须在 _teardown 清空 _active_page **之前**取出句柄：先置空再读会让有 page 的
        # 会话走进「无 page」分支，收尾退化成「起一次性 driver 打开同一 user-data-dir 再关掉」
        # —— 那个循环正被没关的 context 占着，真浏览器下必然超时（实测 30s），
        # 等于每次登录超时都留下 persistent context + node driver 进程泄漏。
        page = self._active_page
        if page is not None:
            # _teardown 自己会清 _active_page；本方法常在「已被取消的协程」里被调用，
            # 收尾由 _teardown 的取消分支负责跑完。
            await self._teardown(page)
            return

        session, self._session = self._session, None
        if session is None:
            return
        account, profile_dir = session
        logger.warning(
            "x login cancelled for %s before browser was ready: closing profile %s", account, profile_dir
        )
        try:
            await self._release_profile(profile_dir)
        except asyncio.CancelledError:
            # 取消上下文里 await 推进不了收尾：登记给驱动层补跑（见 _run_cleanup）
            logger.debug("x login profile release interrupted by cancellation; deferring to driver")
            _run_cleanup(lambda: self._release_profile(profile_dir), what="profile-release")
            # 不上抛：登录结果的语义由 login() 统一落成

    async def _teardown(self, page: Any) -> None:
        """收尾浏览器会话；**在取消路径上也必须真的跑完**（整链超时就是靠取消实现的）。

        ⚠️ 被取消的协程里 **await 任何未就绪的对象都会立刻再抛 CancelledError**，连
        ``asyncio.shield`` 也一样（shield 只让内层任务在外层取消时继续存活，**不会**让外层
        await 多等一秒）。所以「``finally: await teardown()``」在超时路径上等于没收尾：
        persistent context + node driver 进程泄漏，账号的 user-data-dir 还留着 SingletonLock，
        下次登录同一账号直接起不来。

        做法：先直接 await 收尾（正常路径，收尾里的 await 都能跑完）；一旦调用方已处于取消
        上下文（``_in_cancellation``）或收尾本身被取消，把整条收尾链交给 :func:`_run_cleanup`
        —— 登记到驱动登录链的那一层，由它在 ``wait_for`` 返回后跑完。收尾自身的失败只记日志，
        绝不冒泡盖掉登录结果。
        """
        self._active_page = None
        if _in_cancellation():
            # 调用方已处于取消上下文（await 一律不可用）：登记收尾任务，交给驱动层跑完。
            _run_cleanup(lambda: self._teardown_coro(page), what="teardown")
            self._session = None
            return
        try:
            await self._teardown_coro(page)
        except asyncio.CancelledError:
            logger.debug("x login teardown interrupted by cancellation; retrying on a clean loop")
            _run_cleanup(lambda: self._teardown_coro(page), what="teardown")
            # 不上抛：整链超时由调用方（login）落成 LoginResult；这里冒泡只会把「需要人工 /
            # 登录失败」的语义变成进程级异常，skill 服务会当成 500。
        except Exception as exc:  # noqa: BLE001 - 收尾失败只记日志
            logger.warning("x login teardown interrupted: %s", exc)
        finally:
            self._session = None

    async def _release_profile(self, profile_dir: Path) -> None:
        """释放账号的 persistent profile（user-data-dir）。

        Playwright 没有「按目录拿 context」的 API —— 只有 ``launch_persistent_context`` 返回的
        context 对象能 ``close()``。会话句柄拿不到时（取消正好落在起浏览器这一步，对象在别的
        协程栈里已经丢了），只能起一次性 driver 打开同一目录再关掉：这一步是必须的，否则
        Chromium 在该 user-data-dir 上留下的 ``SingletonLock`` 一直存在，**下次登录同一账号
        直接起不来**（起浏览器会因 profile 被占用而失败），等于一次超时把账号钉死。

        收尾失败只记日志：本方法常在超时/取消路径上被调用，冒泡只会盖掉真实登录结果。
        """
        try:
            from playwright.async_api import async_playwright
        except Exception as exc:  # noqa: BLE001 - 没装 playwright 就没有 driver 可收
            logger.debug("playwright unavailable for profile release: %s", exc)
            return

        pw = await async_playwright().start()
        try:
            context = await pw.chromium.launch_persistent_context(str(profile_dir), headless=True)
            await context.close()
            logger.info("released x login profile %s", profile_dir)
        except Exception as exc:  # noqa: BLE001 - 收尾失败只记日志（目录可能压根没被占用）
            logger.warning("release x login profile %s failed: %s", profile_dir, exc)
        finally:
            await self._stop_playwright(pw)

    async def _teardown_coro(self, page: Any) -> None:
        if self.hooks.teardown is not None:
            await self.hooks.teardown(page)
            return
        await self._teardown_browser(page)

    async def _resolve_intervention(
        self, account: str, ops: PageOps, probe_timeout_ms: int
    ) -> tuple[str, str, str]:
        """探测页面要哪种人工介入并取回介入输入，返回 (2FA 码, 二维码路径, 未闭环原因)。

        2FA 与扫码是**两条互斥**的介入路径（X 不会同时要求两者），所以用一次
        有界探测（``probe_timeout_ms``，默认很短）判定走哪条：命中扫码元素 → 落盘
        二维码并问扫码 handler；否则找验证码框 → 问 2FA handler。

        命中判定用 :meth:`PageOps.present`（有界超时 + 只看是否抛异常），
        绝不用 ``wait_for`` 的返回值做真值判定。

        第三个返回值是「介入没闭环」的原因：占位 handler 只声明「需要人工」（返回
        None / 空码）而不抛异常时，这里给出原因，登录链据此把本次登录判成失败
        （needs_manual）—— 不能因为页面后续元素出现，就把没拿到码的扫码算成登录成功。
        """
        if await ops.present(_X_QRCODE_IMG, probe_timeout_ms):
            qrcode_path = await self._take_qrcode(account, ops)
            if not qrcode_path:
                return (
                    "",
                    "",
                    f"account {account!r} requires QR-code login but the QR code "
                    "could not be captured (no manual intervention possible)",
                )
            return "", qrcode_path, ""

        if not await ops.present(_X_OAUTH_VERIFICATION_INPUT, probe_timeout_ms):
            return "", "", ""

        hint = "X asked for a verification code"
        provider = self.hooks.two_factor_provider
        code = provider(account, hint) if provider is not None else self.intervention.on_2fa(account, hint)
        if not (code or "").strip():
            return "", "", f"account {account!r} requires a 2FA code (no handler installed)"

        return self._validate_two_factor_code(code), "", ""

    def _validate_two_factor_code(self, code: str) -> str:
        """校验 2FA 验证码的形状；不合法 → :class:`InterventionRequired`（要求重新介入）。

        两条来源（调用方直传 / handler 取回）都过这里：超 ``two_factor_code_max_len``
        或含非字母数字的串，填进验证码框按 Enter 就是把垃圾提交给 X（连续几次会撞风控）。
        """
        code = (code or "").strip()
        if not code:
            raise InterventionRequired("two_factor", "X asked for a verification code")
        if len(code) > self.config.two_factor_code_max_len or not code.isalnum():
            raise InterventionRequired("two_factor", f"invalid 2FA code shape (len={len(code)})")
        return code

    async def _take_qrcode(self, account: str, ops: PageOps) -> str:
        """扫码登录：**先**把二维码落盘（0600），**再**问 handler，返回真实二维码路径。

        ⚠️ 顺序不能反：handler 常会阻塞等人在手机上扫码（或把码推给 board/Matrix），
        二维码文件必须先存在，介入方才拿得到码。截图/写盘失败时返回空串 = 「没拿到码，
        无法扫码介入」，调用方据此走其它登录方式，绝不返回一个不存在的路径。
        """
        png_path = profile_dir_for(account) / "login-qrcode.png"
        try:
            png = await ops.screenshot(_X_QRCODE_CANVAS)
            if not png:
                raise RuntimeError("empty screenshot")
            self._ensure_dir(png_path.parent)
            png_path.write_bytes(png)
            os.chmod(png_path, 0o600)
        except Exception as exc:  # noqa: BLE001 - 拿不到码就报「无法扫码介入」，不返回假路径
            logger.warning("qrcode screenshot failed for %s: %s", account, exc)
            return ""

        provider = self.hooks.qrcode_provider
        try:
            if provider is not None:
                provider(account, str(png_path))
            else:
                self.intervention.on_qrcode(account, str(png_path))
        except InterventionRequired as exc:
            # 码已经落盘：把路径挂到异常上，登录链据此回报 qrcode_path，
            # 介入方（board / 运维）才知道去哪个文件取码 —— 别让码白拍一张。
            exc.qrcode_path = str(png_path)
            raise
        return str(png_path)

    # —— 内部工具 ——

    @staticmethod
    def _ensure_dir(path: Path) -> None:
        """建目录并把权限收到 0700（profile / 二维码都含登录态相关材料）。"""
        path.mkdir(parents=True, exist_ok=True, mode=0o700)

    def _report(self, result: LoginResult) -> str:
        """回报 AccountManager；关闭回报开关时返回 skipped（登录链本身不受影响）。"""
        if not self.config.notify_account_manager:
            return "skipped: disabled by config"
        return report_to_account_manager(result, self.account_manager)

    # —— Playwright 默认实现（真浏览器）——

    async def _launch_browser(self, profile_dir: Path, headless: bool, chrome_bin: str) -> Any:
        """起持久化 profile 的 Chromium context（按账号目录隔离）并返回一个 page。

        ⚠️ 生命周期自清理：``async_playwright().start()`` 会拉起一个 node driver 子进程，
        之后的任何一步（起浏览器 / 建页）失败都必须先 ``pw.stop()`` 再把异常抛出去 ——
        否则调用方连 page 都拿不到，也就没有 teardown 的机会，每次失败漏一个子进程。
        """
        from playwright.async_api import async_playwright  # 惰性 import：单测不需要 playwright

        pw = await async_playwright().start()
        try:
            launch_kwargs: dict[str, Any] = {"headless": headless}
            if chrome_bin:
                launch_kwargs["executable_path"] = chrome_bin
            context = await pw.chromium.launch_persistent_context(
                str(profile_dir), **launch_kwargs
            )
            page = context.pages[0] if context.pages else await context.new_page()
        except Exception as exc:
            await self._stop_playwright(pw)
            raise RuntimeError(f"launch chromium for x login failed: {exc}") from exc

        # 单动作超时统一由 PageOps.for_timeout 注入，这里只管导航超时
        page.set_default_navigation_timeout(self.config.action_timeout_ms)
        # 把 playwright / context 生命周期挂在 page 上，teardown 时一并收掉
        page._x_login_pw = pw  # type: ignore[attr-defined]
        page._x_login_context = context  # type: ignore[attr-defined]
        return page

    async def _teardown_browser(self, page: Any) -> None:
        """关掉 persistent context 与 playwright driver（顺序：context 先于 driver）。

        ⚠️ 两者的关闭 API **不同**：BrowserContext 是 ``close()``，而 async API 的
        ``Playwright`` 对象只有 ``stop()``（``close()`` 只存在于 Browser/BrowserContext）。
        对 Playwright 调 close() 会抛 AttributeError 并被吞成 warning，结果是每次登录
        都留下一个没停掉的 node driver 进程。
        """
        context = getattr(page, "_x_login_context", None)
        if context is not None:
            try:
                await context.close()
            except Exception as exc:  # noqa: BLE001 - 收尾失败只记日志
                logger.warning("teardown browser context failed: %s", exc)

        pw = getattr(page, "_x_login_pw", None)
        if pw is not None:
            await self._stop_playwright(pw)

    @staticmethod
    async def _stop_playwright(pw: Any) -> None:
        """停掉 playwright driver（async_api.Playwright 只有 stop()）。"""
        try:
            await pw.stop()
        except Exception as exc:  # noqa: BLE001 - 收尾失败只记日志
            logger.warning("teardown playwright driver failed: %s", exc)

class AccountManagerReporter:
    """登录结果 → ``x_accounts`` 表（回报 AccountManager）。

    与 :class:`services.account_manager.AccountManager` 同表同机器隔离口径
    （``machine_id`` + ``username`` 幂等）：登录成功就写回 auth_token/ct0 并把账号
    置为可用（``enabled = 1``、清空 ``last_error``）。MySQL 未就绪（未配 MYSQL_HOST）
    时返回 ``skipped: ...``，登录链照常落盘 session 文件，主链路不受影响。

    ⚠️ 只用 ``x_accounts`` **真实存在**的列。该表由 Go 侧 GORM AutoMigrate 按
    ``apps/core/internal/db/models_content.go`` 的 ``XAccount`` 建，登录回报的落点是
    ``last_login_at`` / ``last_login_error``（M1-3 起随 AutoMigrate 建列；老库由
    ``apps/core/hacks/sql/x_accounts_login_columns.sql`` 幂等补列）。列名两边必须一致：
    对不上会在真库上抛 ``1054 Unknown column``，被这里的 except 吞成 ``failed``，
    表现为「登录回报永远不落库、只在 warning 里露一条 SQL 报错」。
    ⚠️ **失败绝不建行**：``AccountManager.load_eligible`` 只按
    ``enabled = 1 AND machine_id = %s`` 过滤，不校验 token 是否为空。给一个从没登录成功的
    账号插入空 auth_token/ct0 且 enabled=1，等于把它塞进点赞池，之后每次挑到它都必然
    运行时失败。所以：账号不存在 + 登录失败 → 只记日志返回 ``skipped``；账号已存在 +
    登录失败 → 只写 ``last_login_at`` / ``last_login_error``，**绝不覆盖它原有的登录态**。
    """

    SELECT_SQL = (
        "SELECT id FROM x_accounts WHERE machine_id = %s AND username = %s LIMIT 1"
    )
    # 只在登录成功时 INSERT（见 report 的「失败绝不建行」分支）
    INSERT_SQL = (
        "INSERT INTO x_accounts "
        "(machine_id, username, auth_token, ct0, enabled, daily_like_limit, cooldown_minutes, "
        " used_today, last_login_at, last_login_error) "
        "VALUES (%s, %s, %s, %s, 1, %s, %s, 0, NOW(), '')"
    )
    UPDATE_ON_SUCCESS_SQL = (
        "UPDATE x_accounts SET auth_token = %s, ct0 = %s, enabled = 1, "
        " last_login_at = NOW(), last_login_error = '' WHERE id = %s"
    )
    UPDATE_ON_FAILURE_SQL = (
        "UPDATE x_accounts SET last_login_at = NOW(), last_login_error = %s WHERE id = %s"
    )

    def __init__(self, defaults: dict[str, Any] | None = None, machine_id: str | None = None) -> None:
        self.defaults = defaults or {}
        self.machine_id = (
            machine_id if machine_id is not None else os.environ.get("MACHINE_ID", "")
        )

    def report(self, result: LoginResult) -> str:
        """回报一次登录结果，返回可读状态（created / updated / skipped / failed）。

        M1-4 起是**双回报**：先回报自己的 ``x_accounts``（原 M1-3 链路，状态串原样
        作为前缀），再回报 social 的 ``social_accounts``（board 账号管理页
        ``/content/accounts`` 的数据源；board「触发登录」建 login 任务，登录结果由
        这一路 PATCH 回 social 的登录回报端点，驱动它的状态机）。
        ``SOCIAL_LOGIN_REPORT_URL`` 未配置 → social 一路返回 ``skipped``，整体状态串
        与 M1-3 完全一致（行为不变）；配置了才追加 ``; social:<状态>`` 后缀。
        """
        status = self._report_x_accounts(result)

        # 回报桥自身对「未配置 SOCIAL_LOGIN_REPORT_URL」返回 skipped 且零请求，所以
        # **无条件调用**：x_accounts 一路的降级（skipped: MYSQL_HOST not configured）
        # 绝不能顺带跳过 social 一路 —— 账号池是两套（x_accounts 在 core 域、
        # social_accounts 在 social 域），互不依赖，各走各的配置。
        # 经模块属性调用：单测 monkeypatch services.social_report.report_social_login 生效。
        from services import social_report as _social_report

        social = _social_report.report_social_login(
            "x", result.account, result.success, self._message(result)
        )
        if social.startswith("skipped"):
            return status
        return f"{status}; social:{social}"

    @staticmethod
    def _message(result: LoginResult) -> str:
        return result.message or result.intervention or "login failed"

    def _report_x_accounts(self, result: LoginResult) -> str:
        """回报一次登录结果到 x_accounts（created / updated / skipped / failed）。"""
        if not self._mysql_configured():
            return "skipped: MYSQL_HOST not configured"

        username = result.username or result.account
        try:
            conn = self._connect()
            with conn.cursor() as cur:
                cur.execute(self.SELECT_SQL, (self.machine_id, username))
                row = cur.fetchone()

                if row:
                    account_id = int(row["id"])
                    if result.success:
                        cur.execute(
                            self.UPDATE_ON_SUCCESS_SQL, (result.auth_token, result.ct0, account_id)
                        )
                    else:
                        cur.execute(
                            self.UPDATE_ON_FAILURE_SQL,
                            (result.message or result.intervention or "login failed", account_id),
                        )
                    return "updated"

                if not result.success:
                    # 库里没有这个账号 + 这次也没登录成功 → 无登录态可写，绝不建 enabled 空壳行
                    reason = result.message or result.intervention or "login failed"
                    logger.info(
                        "x login report: account %s/%s not in x_accounts and login failed (%s); "
                        "no row created (an enabled row with empty auth_token/ct0 would be picked "
                        "by the account pool)",
                        self.machine_id,
                        username,
                        reason,
                    )
                    return f"skipped: account {username} not registered and login failed"

                cur.execute(
                    self.INSERT_SQL,
                    (
                        self.machine_id,
                        username,
                        result.auth_token,
                        result.ct0,
                        int(self.defaults.get("default_daily_like_limit") or 50),
                        int(self.defaults.get("default_cooldown_minutes") or 10),
                    ),
                )
                return "created"
        except Exception as exc:  # noqa: BLE001 - 回报失败不影响登录本身
            logger.warning("report login to AccountManager failed: %s", exc)
            return f"failed: {exc}"

    def _connect(self) -> Any:
        """取 MySQL 连接（接缝：单测注入 fake 连接；生产走 services.mysql 共享连接）。"""
        from services.mysql import get_mysql

        return get_mysql()

    @staticmethod
    def _mysql_configured() -> bool:
        return bool(os.environ.get("MYSQL_HOST"))


def report_to_account_manager(
    result: LoginResult,
    manager: Any | None = None,
    defaults: dict[str, Any] | None = None,
) -> str:
    """把登录结果回报给 AccountManager。

    ``manager`` 支持三种形态（按顺序探测），让调用方既能传 :class:`AccountManagerReporter`，
    也能传 pyreplay 的 :class:`services.account_manager.AccountManager` 实例：

    1. 有 ``report_login(result)`` → 直接用（AccountManagerReporter / 自定义回报器）；
    2. 有 ``record_login(account, username, auth_token, ct0, success, message)`` → 用它；
    3. 都没有（如当前 AccountManager 尚无登录回报方法）→ 走内置默认回报器。
    """
    reporter = getattr(manager, "report_login", None)
    if callable(reporter):
        try:
            return str(reporter(result))
        except Exception as exc:  # noqa: BLE001 - 回报失败降级为默认回报器
            logger.warning("AccountManager.report_login failed: %s", exc)

    recorder = getattr(manager, "record_login", None)
    if callable(recorder):
        try:
            return str(
                recorder(
                    account=result.account,
                    username=result.username,
                    auth_token=result.auth_token,
                    ct0=result.ct0,
                    success=result.success,
                    message=result.message,
                )
            )
        except Exception as exc:  # noqa: BLE001
            logger.warning("AccountManager.record_login failed: %s", exc)

    return AccountManagerReporter(defaults=defaults).report(result)


class XLoginService:
    """XAdapter.login 的实现体（配置 + handler + hooks + AccountManager 组装）。"""

    def __init__(
        self,
        config: dict[str, Any] | None = None,
        intervention: ManualInterventionHandler | None = None,
        hooks: XLoginHooks | None = None,
        ops: PageOps | None = None,
        account_manager: Any | None = None,
        headless: bool | None = None,
    ) -> None:
        self.config_dict = config or {}
        self.login_config = XLoginConfig.from_config(self.config_dict, headless=headless)
        self.flow = XLoginFlow(
            config=self.login_config,
            intervention=intervention,
            hooks=hooks,
            ops=ops,
            account_manager=account_manager,
        )
        self._account_manager = account_manager

    async def login(
        self,
        account: str,
        password: str = "",
        *,
        headless: bool | None = None,
        force: bool = False,
        two_factor_code: str = "",
    ) -> LoginResult:
        return await self.flow.login(
            account, password, headless=headless, force=force, two_factor_code=two_factor_code
        )

    def report(self, result: LoginResult) -> str:
        return report_to_account_manager(result, self._account_manager, self.config_dict)


def load_x_config() -> dict[str, Any]:
    """读 pyreplay/config/x.json（缺失/损坏返回空 dict，调用方用默认值）。"""
    path = Path(__file__).resolve().parent.parent / "config" / "x.json"
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except (json.JSONDecodeError, OSError) as exc:
        logger.warning("failed to load %s: %s", path, exc)
        return {}


