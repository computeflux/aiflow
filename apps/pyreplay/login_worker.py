#!/usr/bin/env python3
"""social 登录任务消费者（M1-4，工作项 wi-1795685173-65a0）。

闭环位置：board 账号管理页（core-ui ``/content/accounts``）点「触发登录」→ social
``POST /api/social/accounts/{ch}/{id}/login`` 建 ``login`` 任务（social_jobs）并 LPUSH
任务号进 Redis 队列 ``social:jobs:queue`` → **本进程** BRPOPLPUSH 消费 → 执行 X 登录链
（复用 adapters.x_login，登录态按账号隔离落盘）→ 结果回报：

* ``x_accounts``（M1-3 的 AccountManagerReporter，report 内已双回报）；
* social 的 ``social_accounts``（同一 report 里经 services.social_report PATCH 回
  ``PATCH /api/social/accounts/{ch}/{id}/login``，驱动 social 侧状态机）；
* ``social_jobs`` 任务状态机（running → succeeded/failed，本进程直接落库 —— 与
  social 自身搜索链「worker 直接推进 jobs 状态」同一口径，见 apps/social/AGENTS.md）。

运行方式：``python3 login_worker.py``（前台进程，SIGINT/SIGTERM 优雅退出）。
部署形态见 deploy/compose/skills/social-login-worker.yml。

红线：
* 队列里出现**非 login 类型**的任务号（发布/互动任务与本 worker 共用同一队列 key）→
  原样挪回队列尾（BRPOPLPUSH 已把它放进 processing），绝不执行、绝不判失败；
* MySQL/Redis 未配置 → 拒绝启动（消费端没有降级可走，静默空转只会让 board 的登录
  按钮永远无响）；
* 登录结果里的 auth_token/ct0 只进登录链与 x_accounts（M1-3 口径），本进程与 social
  的回报报文里绝不出现明文登录态。
"""

from __future__ import annotations

import json
import logging
import os
import signal
import sys
import threading
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))

import asyncio  # noqa: E402

# pymysql(redis/XAdapter 同理)不在模块顶层 import：单测只测 LoginJobWorker
# （队列/状态机逻辑，MySQL/Redis 全部鸭子注入 _get_job/_update_job/run），干净
# 环境无第三方依赖也能跑；生产在 _get_mysql() 里惰性解析 services.mysql。

logger = logging.getLogger("social.login-worker")

JOB_TYPE_LOGIN = "login"
_JOB_STATUS_RUNNING = "running"
_JOB_STATUS_SUCCEEDED = "succeeded"
_JOB_STATUS_FAILED = "failed"

_SELECT_JOB_SQL = (
    "SELECT id, job_no, job_type, channel, account_id, status, "
    "CAST(payload AS CHAR) AS payload "
    "FROM social_jobs WHERE job_no = %s LIMIT 1"
)
_UPDATE_JOB_SQL = (
    "UPDATE social_jobs SET status = %s, error = %s, attempts = attempts + 1 "
    "WHERE job_no = %s"
)


class LoginJobWorker:
    """login 任务消费者：取队列 → 查任务 → 执行登录 → 回报 → Ack。

    ``login_fn(account, password, headless, force, two_factor_code) -> LoginResult``
    是可注入接缝（单测注入假登录；生产用 XAdapter.login）。
    """

    def __init__(
        self,
        rdb: Any,
        queue_key: str,
        processing_key: str,
        login_fn: Any,
        machine_id: str = "",
        report_fn: Any = None,
    ) -> None:
        self.rdb = rdb
        self.queue_key = queue_key
        self.processing_key = processing_key
        self.login_fn = login_fn
        self.machine_id = machine_id
        # 回报 social 的接缝：默认走 services.social_report（模块属性解析，单测可打桩）。
        self.report_fn = report_fn or self._report_social_default

    # —— 主循环 ——

    def run(self, stop: threading.Event, poll_timeout_sec: int = 5) -> None:
        """阻塞消费循环；stop 置位后在下一个 poll 边界退出（≤poll_timeout_sec 秒）。"""
        logger.info(
            "social login worker started (queue=%s machine_id=%s)",
            self.queue_key,
            self.machine_id or "<empty>",
        )
        while not stop.is_set():
            job_no = self.rdb.brpoplpush(self.queue_key, self.processing_key, timeout=poll_timeout_sec)
            if job_no is None:
                continue
            if isinstance(job_no, bytes):
                job_no = job_no.decode("utf-8", "replace")
            try:
                self.handle_job(str(job_no))
            except Exception as exc:  # noqa: BLE001 - 单任务异常绝不杀消费循环
                logger.error("handle login job %s crashed: %s", job_no, exc)
            finally:
                try:
                    self.rdb.lrem(self.processing_key, 1, job_no)
                except Exception as exc:  # noqa: BLE001
                    logger.warning("ack job %s failed: %s", job_no, exc)

    # —— 单任务处理（每步都可见，不留幽灵状态）——

    def handle_job(self, job_no: str) -> str:
        job = self._get_job(job_no)
        if job is None:
            logger.warning("login job %s not in social_jobs (已删除/脏任务号)，丢弃", job_no)
            return "dropped"

        if job["job_type"] != JOB_TYPE_LOGIN:
            # 非本 worker 的任务类型：挪回队列尾（FIFO 保持），交给真正的消费方。
            # 判 failed 会替别的 worker 把任务写死，绝不。
            self.rdb.lrem(self.processing_key, 1, job_no)
            self.rdb.lpush(self.queue_key, job_no)
            logger.info("job %s type=%s 非 login，已挪回队列尾", job_no, job["job_type"])
            return "requeued"

        if job["status"] != "queued":
            # processing 队列里残留的 running 任务 = 上次消费进程崩溃后的重投。
            # attempts 已自增过，这里按「重投」继续执行（登录幂等：成功即刷新登录态）。
            logger.info("login job %s status=%s（重投），继续执行", job_no, job["status"])

        self._update_job(job_no, _JOB_STATUS_RUNNING, "")

        payload = _parse_payload(job.get("payload"))
        account = str(payload.get("account_id") or payload.get("username") or job["account_id"])
        password = str(payload.get("password") or "")  # board 触发登录不带密码（登录态走人工/复用链）

        try:
            outcome = self.login_fn(
                account,
                password,
                headless=True,
                force=bool(payload.get("force", True)),
                two_factor_code=str(payload.get("two_factor_code") or ""),
            )
            # XAdapter.login 是协程（登录链整条是 async）：消费循环是同步的，
            # 每个任务一个事件循环跑完即散（登录是分钟级长任务，不共用循环）。
            result = asyncio.run(outcome) if asyncio.iscoroutine(outcome) else outcome
        except Exception as exc:  # noqa: BLE001 - 登录链异常也变成失败回报，不冒泡
            logger.error("login execution for job %s crashed: %s", job_no, exc)
            self._update_job(job_no, _JOB_STATUS_FAILED, f"登录执行异常: {exc}"[:500])
            return "failed"

        message = str(getattr(result, "message", "") or "")

        # 回报 social 账号池（board 账号管理页的状态机数据源）。**显式按任务回报**：
        # 登录链内部的默认回报器（AccountManagerReporter 双回报）只在 MYSQL_HOST 已配时
        # 才会被走到（它先判 MySQL，未配置时根本到不了 social 一路），消费端不能依赖
        # 那条隐式链路，必须在这里对 login 任务显式回报一次。两侧都读同一个
        # SOCIAL_LOGIN_REPORT_URL：未配置时 report_social_login 返回 skipped 且零请求；
        # 配置了则可能重复回报一次 —— social 侧回报是幂等的状态机推进（同结果重复
        # 回报落同一状态、last_login_at 刷新），可接受。
        social_status = self.report_fn(job["channel"], job["account_id"], result)

        if bool(getattr(result, "success", False)):
            self._update_job(job_no, _JOB_STATUS_SUCCEEDED, "")
            logger.info("login job %s succeeded (account=%s social=%s)", job_no, account, social_status)
            return "succeeded"

        reason = message or str(getattr(result, "intervention", "") or "") or "login failed"
        self._update_job(job_no, _JOB_STATUS_FAILED, reason[:500])
        logger.info("login job %s failed: %s (social=%s)", job_no, reason, social_status)
        return "failed"

    @staticmethod
    def _report_social_default(channel: str, account_id: str, result: Any) -> str:
        """把登录结果回报 social 的账号池（成功/失败都回报；报文只出摘要字段）。"""
        from services import social_report

        return social_report.report_social_login(
            channel,
            account_id,
            bool(getattr(result, "success", False)),
            str(getattr(result, "message", "") or getattr(result, "intervention", "") or "login failed"),
        )

    # —— MySQL 接缝（单测可整体替换这两个方法）——

    def _get_job(self, job_no: str) -> dict[str, Any] | None:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(_SELECT_JOB_SQL, (job_no,))
            row = cur.fetchone()
        return dict(row) if row else None

    def _update_job(self, job_no: str, status: str, err: str) -> None:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(_UPDATE_JOB_SQL, (status, err, job_no))


def _get_mysql() -> Any:
    """MySQL 连接接缝（生产 = services.mysql 共享连接；单测整体替换 _get_job/_update_job）。"""
    from services.mysql import get_mysql

    return get_mysql()


def _parse_payload(raw: Any) -> dict[str, Any]:
    if isinstance(raw, dict):
        return raw
    if not raw:
        return {}
    try:
        data = json.loads(raw)
        return data if isinstance(data, dict) else {}
    except (TypeError, ValueError):
        return {}


def main() -> None:
    import redis

    from adapters.x_adapter import XAdapter  # 登录链（playwright 依赖链，生产进程才需要）

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
        stream=sys.stdout,
    )

    if not os.environ.get("MYSQL_HOST"):
        logger.error("MYSQL_HOST 未配置：登录消费端没有降级可走，拒绝启动")
        sys.exit(1)

    queue_key = os.environ.get("SOCIAL_REDIS_QUEUE", "social:jobs:queue")
    processing_key = queue_key + ":processing"

    # 连接参数与 social 侧 store.OpenQueue 逐一对齐（REDIS_URL 整串优先，否则
    # REDIS_ADDR host:port + REDIS_PASSWORD；默认 redis:6379）——生产两端必须连
    # 同一个队列，参数口径不能各写各的。
    url = os.environ.get("REDIS_URL")
    if url:
        rdb = redis.Redis.from_url(url, decode_responses=False)
    else:
        host, _, port = os.environ.get("REDIS_ADDR", "redis:6379").partition(":")
        rdb = redis.Redis(
            host=host or "redis",
            port=int(port) if port else 6379,
            password=os.environ.get("REDIS_PASSWORD") or None,
            decode_responses=False,
        )

    # 启动即探活：连不上就失败可见（restart 策略会带退避重连），不静默空转。
    try:
        rdb.ping()
    except Exception as exc:  # noqa: BLE001
        logger.error("Redis 连接失败（%s）：拒绝启动", exc)
        sys.exit(1)

    x_adapter = XAdapter()

    def _login(account: str, password: str, **kw: Any) -> Any:
        # 返回协程对象交给 worker（worker 里 asyncio.run 消费）；XAdapter.login 本体是 async。
        return x_adapter.login(account, password, **kw)

    worker = LoginJobWorker(
        rdb=rdb,
        queue_key=queue_key,
        processing_key=processing_key,
        login_fn=_login,
        machine_id=os.environ.get("MACHINE_ID", ""),
    )

    stop = threading.Event()

    def _on_signal(signum: int, _frame: Any) -> None:
        logger.info("received signal %s, shutting down", signum)
        stop.set()

    signal.signal(signal.SIGINT, _on_signal)
    signal.signal(signal.SIGTERM, _on_signal)

    worker.run(stop)
    logger.info("social login worker stopped")


if __name__ == "__main__":
    main()
