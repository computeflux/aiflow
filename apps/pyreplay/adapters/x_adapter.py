"""X (Twitter) 登录入口（M1-3，工作项 wi-1795586435-e694）。

``XAdapter`` 是 pyreplay 侧的 X 账号登录门面：把 pyreplay 的 X 登录逻辑收口成
一个稳定入口，供 Temporal worker（main.py）、skill 服务与账号运维脚本调用。

它只做**装配**，登录链本身在 :mod:`adapters.x_login`：

    adapter = XAdapter()                       # 读 config/x.json
    result = await adapter.login("alice@example.com", "pw", two_factor_code="123456")
    result.success, result.session_file, result.account_manager

``login`` 的行为契约（详见 x_login.XLoginFlow.login）：

* headless / headed 由构造参数或 ``login(headless=...)`` 决定（扫码登录走 headed）；
* profile 目录与 session 文件**按账号隔离**持久化（``X_PROFILE_DIR`` / ``X_SESSION_DIR``）；
* 2FA / 扫码是**人工介入占位接口**：未装 handler 时不挂死，直接返回
  ``needs_manual=True`` + ``intervention``（two_factor / qrcode）+ 二维码落盘路径；
* 登录结果回报 AccountManager（写 ``x_accounts``；MySQL 未就绪时降级 ``skipped``）。
"""

from __future__ import annotations

from typing import Any

from adapters.x_login import (
    AccountManagerReporter,
    CallbackIntervention,
    LoginResult,
    ManualInterventionHandler,
    NoIntervention,
    XLoginConfig,
    XLoginFlow,
    XLoginHooks,
    XSession,
    load_session,
    load_x_config,
    profile_dir_for,
    save_session,
    session_file_for,
)

__all__ = [
    "AccountManagerReporter",
    "CallbackIntervention",
    "LoginResult",
    "ManualInterventionHandler",
    "NoIntervention",
    "XAdapter",
    "XLoginConfig",
    "XLoginFlow",
    "XLoginHooks",
    "XSession",
    "load_session",
    "profile_dir_for",
    "save_session",
    "session_file_for",
]


class XAdapter:
    """X 账号适配器（登录 + 登录态读写）。

    Args:
        config: 显式配置字典；None = 读 ``config/x.json``。
        headless: 覆盖配置的无头开关（扫码登录传 False）。
        intervention: 人工介入 handler（2FA / 扫码）；None = :class:`NoIntervention`。
        account_manager: AccountManager（回报登录结果）；None = 内置默认回报器。
        hooks: 浏览器接缝（测试/自定义浏览器实现注入点）。
    """

    def __init__(
        self,
        config: dict[str, Any] | None = None,
        headless: bool | None = None,
        intervention: ManualInterventionHandler | None = None,
        account_manager: Any | None = None,
        hooks: XLoginHooks | None = None,
    ) -> None:
        self.config = load_x_config() if config is None else config
        self.headless = self.config.get("headless", True) if headless is None else headless
        self.intervention = intervention or NoIntervention()
        self.account_manager = account_manager
        self.login_config = XLoginConfig.from_config(self.config, headless=self.headless)
        self.login_flow = XLoginFlow(
            config=self.login_config,
            intervention=self.intervention,
            hooks=hooks,
            account_manager=account_manager,
        )

    async def login(
        self,
        account: str,
        password: str = "",
        *,
        headless: bool | None = None,
        force: bool = False,
        two_factor_code: str = "",
    ) -> LoginResult:
        """登录一个 X 账号：登录 → 按需人工介入 → 按账号持久化 → 回报 AccountManager。"""
        return await self.login_flow.login(
            account,
            password,
            headless=headless,
            force=force,
            two_factor_code=two_factor_code,
        )

    # —— 登录态读写（供点赞/回复等执行链复用，不必每次重登）——

    def session(self, account: str) -> XSession | None:
        """读账号已持久化的登录态（无/损坏 = None）。"""
        return load_session(account)

    def save_session(self, account: str, session: XSession) -> str:
        """写账号登录态文件（0600），返回路径。"""
        return str(save_session(account, session))

    def profile_dir(self, account: str) -> str:
        """账号专属浏览器 profile 目录（按账号隔离，Chromium user-data-dir）。"""
        return str(profile_dir_for(account))

    def session_file(self, account: str) -> str:
        """账号专属登录态文件路径。"""
        return str(session_file_for(account))


def make_intervention(
    two_factor: Any | None = None,
    qrcode: Any | None = None,
) -> ManualInterventionHandler:
    """便捷构造人工介入 handler（回调式；缺省的介入类型仍会明确报「需要人工」）。"""
    return CallbackIntervention(two_factor=two_factor, qrcode=qrcode)
