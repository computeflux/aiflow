"""social 账号登录回报桥（M1-4，工作项 wi-1795685173-65a0）。

pyreplay 的 X 登录链（:class:`adapters.x_login.XLoginFlow`）原本只回报**自己的**
``x_accounts`` 账号池（:class:`adapters.x_login.AccountManagerReporter`）。M1-4 的
账号管理页（core-ui ``/content/accounts``）管的是 **social 的 social_accounts**：
board 点「触发登录」→ social 建 ``login`` 任务入 Redis 队列 → worker 消费执行 →
把结果 PATCH 回 social 的
``PATCH /api/social/accounts/{channel}/{account_id}/login``（body ``{success, message}``），
由 social 侧 ``internal/loginreport`` 判定并推进状态机（active / need_login / banned…）。

本模块补的就是最后这一段「回报 social」：

* **纯 stdlib**（urllib）：pyreplay 的依赖里已有 requests，但登录回报是收尾动作，
  跑在 skill 服务线程里，任何一次网络抖动都不许冒泡打崩登录链 —— 全部异常吞成
  ``skipped/failed`` 状态字符串，与 AccountManagerReporter 的降级口径一致。
* **未配置即整体跳过**：``SOCIAL_LOGIN_REPORT_URL`` 为空 → :func:`report_social_login`
  返回 ``skipped: SOCIAL_LOGIN_REPORT_URL not configured``，登录链行为与 M1-3 完全一致。
* **message 脱敏由调用方保证**：本模块只搬运 :meth:`LoginResult.summary` 的字段
  （auth_token/ct0 只出掩码），绝不在这里读任何登录态明文。

环境变量：
* ``SOCIAL_LOGIN_REPORT_URL``：social 服务基址（如 ``http://social:8010``；容器内
  走 workflow-net 内部网络，与 skill invoke 同一信任域，social 账号端点无鉴权）。
* ``SOCIAL_LOGIN_REPORT_TIMEOUT``：单次回报超时秒数（默认 10）。
"""

from __future__ import annotations

import json
import logging
import os
import urllib.error
import urllib.request
from typing import Any

logger = logging.getLogger(__name__)

ENV_REPORT_URL = "SOCIAL_LOGIN_REPORT_URL"
ENV_REPORT_TIMEOUT = "SOCIAL_LOGIN_REPORT_TIMEOUT"

# social 的渠道账号标识约定与 pyreplay 的 machine_id 对齐：channel = "x"（X 渠道），
# account_id = 登录用的账号名（LoginResult.account）。channel/account_id 拼进 URL 前
# 必须做百分号编码：账号名允许出现中文/空格等，直接拼接会打出畸形 URL。


def report_social_login(
    channel: str,
    account_id: str,
    success: bool,
    message: str = "",
    base_url: str | None = None,
    timeout: float | None = None,
    opener: Any = None,
) -> str:
    """把一次登录结果回报给 social 的账号池，返回可读状态。

    状态取值（与 AccountManagerReporter.report 同风格，调用方原样放进登录摘要）：
      * ``skipped: ...`` —— 未配置回报地址 / channel+account_id 为空（不发请求）；
      * ``updated``      —— social 返回 2xx（状态机已按其 loginreport 判定推进）；
      * ``failed: ...``  —— 网络/HTTP 错误（登录链本身不受影响，只记日志）。
    """
    base = (base_url if base_url is not None else os.environ.get(ENV_REPORT_URL, "")).strip().rstrip("/")
    if not base:
        return f"skipped: {ENV_REPORT_URL} not configured"

    channel = (channel or "").strip()
    account_id = (account_id or "").strip()
    if not channel or not account_id:
        return "skipped: channel / account_id 必填"

    url = f"{base}/api/social/accounts/{_quote(channel)}/{_quote(account_id)}/login"
    body = json.dumps(
        {"success": bool(success), "message": (message or "").strip()}
    ).encode("utf-8")

    if timeout is None:
        raw = os.environ.get(ENV_REPORT_TIMEOUT, "").strip()
        try:
            timeout = float(raw) if raw else 10.0
        except ValueError:
            timeout = 10.0

    req = urllib.request.Request(  # noqa: S310 - scheme 由配置保证（http://<内网主机>:8010）
        url,
        data=body,
        headers={"Content-Type": "application/json"},
        method="PATCH",
    )
    open_with = opener or urllib.request.urlopen
    try:
        with open_with(req, timeout=timeout) as resp:
            status = getattr(resp, "status", None) or resp.getcode()
            resp.read()  # 必须读完，连接才能干净归还
    except urllib.error.HTTPError as exc:  # 4xx/5xx：social 明确拒收（404 账号不存在等）
        detail = _read_error_body(exc)
        logger.warning("report login to social failed: HTTP %s %s (%s)", exc.code, url, detail)
        return f"failed: HTTP {exc.code} {detail}"
    except Exception as exc:  # noqa: BLE001 - 回报失败绝不影响登录本身
        logger.warning("report login to social failed: %s (%s)", exc, url)
        return f"failed: {exc}"

    if 200 <= int(status) < 300:
        return "updated"
    return f"failed: HTTP {status}"


def _quote(v: str) -> str:
    from urllib.parse import quote

    return quote(v, safe="")


def _read_error_body(exc: urllib.error.HTTPError) -> str:
    """尽力从错误响应里取出 social 的 error 文本（截断，日志/状态串用）。"""
    try:
        raw = exc.read(2048).decode("utf-8", "replace")
        data = json.loads(raw)
        return str(data.get("error") or data.get("reason") or raw)[:300]
    except Exception:  # noqa: BLE001
        return ""
