#!/usr/bin/env python3
"""'like' skill 服务：HTTP 包装 XLikeAdapter（P1 Skill 框架 pilot，docs/plan/transform-plan.md §6.6）。

与 main.py 的 Temporal worker 同容器运行（entrypoint.sh 后台拉起），监听 9500 端口。

接口：
    GET  /health              -> 200 {"ok": true}
    POST /invoke              body: {"post_url": "...", "like_count": 1}
        -> 200 {"ok": true, "tweet_id": "...", "attempts": N, "success_count": M,
                "results": [{"tweet_id", "success", "error"}]}
        业务失败也返回 200 + ok=false（由 Gateway 依据顶层 ok 字段判定状态），
        进程级错误（非法请求体等）返回 500。

约束：
    - like_count 上限 20（同一推文用不同账号点 N 次），账号级去重；
    - 账号使用后 mark_used（计数 + 冷却），复用 services.account_manager；
    - 结果写 like_records（与 main.py._save_result 同表同结构）；
    - 进程内串行执行点赞（XLikeAdapter 单浏览器会话，并发会互相干扰）。
"""
from __future__ import annotations

import asyncio
import json
import logging
import os
import sys
import threading
import traceback
from datetime import UTC, datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

from adapters.models import LikeResult  # noqa: E402
from adapters.x_adapter import XAdapter  # noqa: E402
from adapters.x_like import XLikeAdapter  # noqa: E402
from adapters.x_login import LoginResult  # noqa: E402
from services.account_manager import AccountManager  # noqa: E402
from services.like_skill import LikeSkill  # noqa: E402
from services.mysql import get_mysql  # noqa: E402

logger = logging.getLogger("skill.like")

LISTEN_HOST = os.environ.get("SKILL_LIKE_HOST", "0.0.0.0")
LISTEN_PORT = int(os.environ.get("SKILL_LIKE_PORT", "9500"))
_MACHINE_ID = os.environ.get("MACHINE_ID", "")


# 与 main.py 共用账号池逻辑；XLikeAdapter 每进程一个实例（内部浏览器会话）
_account_manager = AccountManager()
_x_like = XLikeAdapter()
# X 登录链（M1-3）：登录结果回报同一个 AccountManager（写 x_accounts）
_x_adapter = XAdapter(account_manager=_account_manager)


class _AccountPool:
    """LikeSkill 的账号池适配器：pick_account → pick_exclude_many（忽略每日限额）。

    技能内核只认「取一个没用过的账号 / 用完记账」两个动作，账号池的冷却与每日限额
    策略留在 services.account_manager 里，两边各自演进互不影响。
    """

    def pick_account(self, excluded: set[int] | None = None) -> Any:
        return _account_manager.pick_exclude_many(excluded)

    def mark_used(self, account_id: int) -> None:
        _account_manager.mark_used(account_id)


class _SkillStore:
    """LikeSkill 的落库适配器：like_records（逐条）+ error_logs（技能级失败）。"""

    def save_like_record(self, result: LikeResult, account: Any = None) -> None:
        """写 like_records（与 main.py._save_result 同一表结构）。"""
        conn = get_mysql()
        sql = (
            "INSERT INTO like_records "
            "(tweet_id, tweet_url, success, error, liked_at, created_at, "
            " machine_id, account_id, account_username) "
            "VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)"
        )
        now = datetime.now(UTC).replace(tzinfo=None)
        with conn.cursor() as cur:
            cur.execute(sql, (
                result.tweet_id,
                result.tweet_url,
                1 if result.success else 0,
                result.error or "",
                result.liked_at.replace(tzinfo=None) if result.liked_at else now,
                now,
                (_MACHINE_ID or "")[:64],
                account.id if account is not None else None,
                (account.username or "")[:128] if account is not None else "",
            ))

    def log_error(self, source: str, message: str, context: str = "") -> None:
        """写 error_logs 统一错误表（board 错误日志页可查看）。"""
        conn = get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "INSERT INTO error_logs "
                "(system, level, source, message, context, created_at) "
                "VALUES (%s, %s, %s, %s, %s, %s)",
                (
                    "pyreplay-skill",
                    "error",
                    source,
                    message[:8000],
                    context or "{}",
                    datetime.now(UTC).replace(tzinfo=None),
                ),
            )


def _save_error(detail: str) -> None:
    """进程级异常（非法请求体、协程崩溃等）落 error_logs。"""
    try:
        _SkillStore().log_error("skill.like", detail)
    except Exception as exc:  # noqa: BLE001
        logger.warning("save error log failed: %s", exc)


# skill 'like' 的内核实例（进程内单例：XLikeAdapter 的浏览器会话是进程级资源）
_like_skill = LikeSkill(
    liker=_x_like,
    accounts=_AccountPool(),
    store=_SkillStore(),
    machine_id=_MACHINE_ID,
)

# XLikeAdapter 底层是单浏览器会话，进程内串行执行点赞；登录同样串行（共用浏览器资源）
_LIKE_LOCK = threading.Lock()
_LOGIN_LOCK = threading.Lock()


def _do_login(
    account: str,
    password: str = "",
    headless: bool | None = None,
    force: bool = False,
    two_factor_code: str = "",
) -> dict[str, Any]:
    """登录一个 X 账号并把登录态按账号隔离持久化（详见 adapters.x_login.XLoginFlow）。

    2FA / 扫码的**人工介入占位接口**：本进程没有接介入渠道（Matrix / board 审批），
    所以未装 handler 时登录链直接回报 needs_manual + intervention + 二维码落盘路径，
    调用方（board / 运维）拿到后走线下介入再带 two_factor_code 重试。
    """
    result: LoginResult = asyncio.run(
        _x_adapter.login(
            account,
            password,
            headless=headless,
            force=force,
            two_factor_code=two_factor_code,
        )
    )
    return result.summary()

def _do_like(post_url: str, like_count: Any = None) -> dict[str, Any]:
    """对一条推文用不同账号点赞 like_count 次（账号级去重）。

    实现收口在 :class:`services.like_skill.LikeSkill`（skill schema 的唯一实现）；
    本进程只负责提供三条依赖：账号池（AccountManager）、点赞执行器（XLikeAdapter，
    协程 → asyncio.run 收敛）、落库（like_records + error_logs）。
    """
    return asyncio.run(_like_skill.run(post_url, like_count))

class _Handler(BaseHTTPRequestHandler):
    server_version = "like-skill/1.0"
    protocol_version = "HTTP/1.1"

    def _send_json(self, code: int, payload: dict[str, Any]) -> None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:  # noqa: N802
        if self.path.split("?")[0] == "/health":
            self._send_json(200, {"ok": True})
        else:
            self._send_json(404, {"ok": False, "reason": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        path = self.path.split("?")[0]
        if path == "/login":
            self._handle_login()
        elif path == "/invoke":
            self._handle_invoke()
        else:
            self._send_json(404, {"ok": False, "reason": "not found"})

    def _handle_login(self) -> None:
        """POST /login —— X 账号登录（M1-3）。

        body: {"account": "...", "password": "***", "headless": true,
               "force": false, "two_factor_code": ""}
             -> 200 登录结果摘要（**脱敏**：auth_token/ct0 只出掩码）：
                {"ok", "account", "username", "needs_manual", "intervention",
                 "qrcode_path", "message", "session_file", "profile_dir", "source",
                 "account_manager", "auth_token": "<masked>", "ct0": "<masked>"}

        业务失败（含 needs_manual=true 的 2FA/扫码介入）也返回 200 + ok=false，
        进程级错误（非法请求体等）才 500。
        """
        try:
            body = self._read_json_body()
            account = str(body.get("account") or "").strip()
            if not account:
                raise ValueError("account is required")

            headless_raw = body.get("headless")
            headless = None if headless_raw is None else bool(headless_raw)

            # 登录会起浏览器会话，与点赞分锁串行，避免与 worker 的会话互相干扰
            with _LOGIN_LOCK:
                result = _do_login(
                    account,
                    str(body.get("password") or ""),
                    headless=headless,
                    force=bool(body.get("force")),
                    two_factor_code=str(body.get("two_factor_code") or ""),
                )
            self._send_json(200, {"ok": bool(result.get("success")), **result})
        except Exception as exc:
            detail = f"{exc}\n{traceback.format_exc()}"
            logger.error("x login skill crashed: %s", detail)
            _save_error(detail)
            self._send_json(500, {"ok": False, "reason": str(exc)[:500]})

    def _read_json_body(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length > 0 else b"{}"
        body = json.loads(raw or b"{}")
        if not isinstance(body, dict):
            raise ValueError("body must be a JSON object")
        return body

    def _handle_invoke(self) -> None:
        try:
            body = self._read_json_body()

            post_url = str(body.get("post_url") or "")
            if not post_url:
                raise ValueError("post_url is required")
            # like_count 的规整（缺省 1 / 上限 MAX_LIKE_COUNT）由内核统一负责，
            # 与 Temporal activity、后台 like_jobs 执行器同一口径。

            # 串行执行：避免与 Temporal worker 的浏览器会话互相干扰
            with _LIKE_LOCK:
                result = _do_like(post_url, body.get("like_count"))
            # 业务失败也 200：ok=false 由 Gateway 的 output 校验判定
            self._send_json(200, result)
        except Exception as exc:
            detail = f"{exc}\n{traceback.format_exc()}"
            logger.error("like skill crashed: %s", detail)
            _save_error(detail)
            self._send_json(500, {"ok": False, "reason": str(exc)[:500]})

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A003
        logger.debug("skill http: %s", fmt % args)


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
        stream=sys.stdout,
    )
    server = ThreadingHTTPServer((LISTEN_HOST, LISTEN_PORT), _Handler)
    logger.info(
        "like skill server listening on %s:%d (machine_id=%s) [routes: /invoke /login /health]",
        LISTEN_HOST, LISTEN_PORT, _MACHINE_ID or "default",
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
