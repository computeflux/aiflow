"""Temporal Python worker for X/Twitter like/replay operations."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import random
import traceback
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import pymysql
from dotenv import load_dotenv
from temporalio import activity
from temporalio.client import Client
from temporalio.contrib.opentelemetry import TracingInterceptor
from temporalio.worker import (
    ActivityInboundInterceptor,
    ExecuteActivityInput,
    Interceptor,
    Worker,
)

from adapters.models import LikeResult, LikeTarget, XAccount
from adapters.x_like import XLikeAdapter
from services.account_manager import AccountManager
from telemetry import current_trace_id, init_tracing, traced

load_dotenv(Path(__file__).resolve().parent / ".env")

# 本机（机器人）标识：执行器只处理 like_jobs 里 machine_id 等于本机的任务
_MACHINE_ID = os.environ.get("MACHINE_ID", "")

# ---------------------------------------------------------------------------
# Colored logging
# ---------------------------------------------------------------------------

_COLORS = {
    logging.DEBUG: "\033[0;36m",
    logging.INFO: "\033[0;32m",
    logging.WARNING: "\033[1;33m",
    logging.ERROR: "\033[0;31m",
    logging.CRITICAL: "\033[1;31m",
}
_RESET = "\033[0m"


class _ColoredFormatter(logging.Formatter):
    def format(self, record):
        color = _COLORS.get(record.levelno, "")
        record.levelname = f"{color}{record.levelname}{_RESET}"
        return super().format(record)


def _setup_logging():
    handler = logging.StreamHandler()
    handler.setFormatter(_ColoredFormatter(
        fmt="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        datefmt="%H:%M:%S",
    ))
    logging.root.handlers = [handler]
    logging.root.setLevel(logging.INFO)


_setup_logging()

logger = logging.getLogger(__name__)


def _load_x_json() -> dict[str, Any]:
    path = Path(__file__).resolve().parent / "config" / "x.json"
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return {}


_x_config = _load_x_json()

# ---------------------------------------------------------------------------
# Adapters & services
# ---------------------------------------------------------------------------

_x_like = XLikeAdapter()
_account_manager = AccountManager()

# 随机间隔配置（秒），避免固定节奏被封号
_MIN_DELAY = float(_x_config.get("min_delay_between_likes", 30))
_MAX_DELAY = float(_x_config.get("max_delay_between_likes", 180))
# 分发窗口（小时）：like_jobs 的 scheduled_at 在 now ~ now+window 内随机分布
_LIKE_WINDOW_HOURS = float(_x_config.get("like_jobs_window_hours", 24) or 24)
# 每个订阅目标每次分发扫描的推文条数
_PROFILE_SCAN_LIMIT = int(_x_config.get("profile_scan_limit", 50) or 50)
# 只分发 published_at 在最近 N 天内的推文（避免点赞过旧内容）
_LIKE_TWEET_MAX_AGE_DAYS = float(_x_config.get("like_tweet_max_age_days", 7) or 7)
# 任务失败后的最大尝试次数
_MAX_JOB_ATTEMPTS = 5
# 一次点赞会话最多点赞条数（随机 1~N，模拟真人一次浏览点多个帖子）
_LIKE_BATCH_MAX = int(_x_config.get("like_batch_size_max", 3) or 3)


def _random_delay() -> float:
    return random.uniform(_MIN_DELAY, _MAX_DELAY)


# ---------------------------------------------------------------------------
# MySQL connection (lazy, cached per worker process)
# ---------------------------------------------------------------------------

_mysql_conn: Any | None = None


def _get_mysql() -> Any:
    global _mysql_conn
    if _mysql_conn is None:
        _mysql_conn = _connect_mysql()
    try:
        # 长空闲后 MySQL 会断开连接（wait_timeout），ping 失败时自动重连
        _mysql_conn.ping()
    except Exception:
        try:
            _mysql_conn.close()
        except Exception:
            pass
        _mysql_conn = _connect_mysql()
    return _mysql_conn


def _connect_mysql() -> Any:
    return pymysql.connect(
        host=os.environ["MYSQL_HOST"],
        port=int(os.environ["MYSQL_PORT"]),
        user=os.environ["MYSQL_USER"],
        password=os.environ["MYSQL_PASSWORD"],
        database=os.environ["MYSQL_DATABASE"],
        charset="utf8mb4",
        autocommit=True,
        cursorclass=pymysql.cursors.DictCursor,
    )


# ---------------------------------------------------------------------------
# 统一错误日志：写入 error_logs 表，board 的“错误日志”页查看（system=pyreplay）
# ---------------------------------------------------------------------------

_SYSTEM_NAME = "pyreplay"


def _log_error(
    source: str,
    message: str,
    workflow_id: str = "",
    run_id: str = "",
    stack: str = "",
    context: str = "",
    trace_id: str = "",
) -> None:
    """尽力而为地把错误写入共享的 error_logs 表，失败只打日志不影响主流程。"""
    try:
        conn = _get_mysql()
        sql = (
            "INSERT INTO error_logs "
            "(`system`, level, source, workflow_id, run_id, message, stack, context, trace_id, resolved, created_at) "
            "VALUES (%s, 'error', %s, %s, %s, %s, %s, %s, %s, 0, NOW())"
        )
        with conn.cursor() as cur:
            cur.execute(sql, (
                _SYSTEM_NAME,
                (source or "")[:128],
                (workflow_id or "")[:255],
                (run_id or "")[:255],
                (message or "")[:4000],
                (stack or "")[:60000],
                (context or "")[:60000],
                (trace_id or current_trace_id())[:64],
            ))
    except Exception as exc:
        logger.warning("failed to save error log: %s", exc)


def _ensure_error_logs_table() -> None:
    """在 Go 侧尚未建表时兜底创建 error_logs（与 GORM 迁移后的结构兼容）。"""
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "CREATE TABLE IF NOT EXISTS error_logs ("
                " id BIGINT AUTO_INCREMENT PRIMARY KEY,"
                " `system` VARCHAR(64) NOT NULL DEFAULT '',"
                " level VARCHAR(16) NOT NULL DEFAULT 'error',"
                " source VARCHAR(128) NOT NULL DEFAULT '',"
                " workflow_id VARCHAR(255) NOT NULL DEFAULT '',"
                " run_id VARCHAR(255) NOT NULL DEFAULT '',"
                " message TEXT NOT NULL,"
                " stack LONGTEXT NULL,"
                " context LONGTEXT NULL,"
                " trace_id VARCHAR(64) NOT NULL DEFAULT '',"
                " resolved TINYINT(1) NOT NULL DEFAULT 0,"
                " created_at DATETIME(3) NULL,"
                " resolved_at DATETIME(3) NULL,"
                " INDEX idx_error_logs_system (`system`),"
                " INDEX idx_error_logs_level (level),"
                " INDEX idx_error_logs_source (source),"
                " INDEX idx_error_logs_workflow_id (workflow_id),"
                " INDEX idx_error_logs_resolved (resolved),"
                " INDEX idx_error_logs_created_at (created_at)"
                ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
            )
    except Exception as exc:
        logger.warning("failed to ensure error_logs table: %s", exc)


class _ErrorLogActivityInbound(ActivityInboundInterceptor):
    """捕获 activity 抛出的异常并写入 error_logs，然后照常抛出让 Temporal 重试。"""

    async def execute_activity(self, input: ExecuteActivityInput) -> Any:
        try:
            return await super().execute_activity(input)
        except Exception as exc:
            try:
                info = activity.info()
                _log_error(
                    source=info.activity_type,
                    workflow_id=info.workflow_id or "",
                    run_id=info.workflow_run_id or "",
                    message=str(exc),
                    stack=traceback.format_exc(),
                    context=(
                        '{"attempt":%d,"activity_id":"%s"}'
                        % (info.attempt, info.activity_id or "")
                    ),
                )
            except Exception:
                pass
            raise


class ErrorLogInterceptor(Interceptor):
    def intercept_activity(
        self, next: ActivityInboundInterceptor
    ) -> ActivityInboundInterceptor:
        return _ErrorLogActivityInbound(next)


def _save_result(
    result: LikeResult,
    machine_id: str = "",
    account: XAccount | None = None,
) -> None:
    """Persist like result to MySQL (shared by all activities/executor).

    machine_id 记录是哪台机器点的，account 记录用哪个账号点的——
    同一推文允许多机器多账号重复点赞（账号级去重）。
    """
    try:
        conn = _get_mysql()
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
                (machine_id or "")[:64],
                account.id if account is not None else None,
                (account.username or "")[:128] if account is not None else "",
            ))
    except Exception as exc:
        logger.warning("failed to save like result: %s", exc)


def _ensure_like_records_table() -> None:
    """兜底创建 like_records（含 machine_id / account 列，账号级去重）。

    老表若缺少新增列则通过 information_schema 检查后 ALTER 补齐
    （MySQL 8 不支持 ADD COLUMN IF NOT EXISTS）。
    """
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "CREATE TABLE IF NOT EXISTS like_records ("
                " id BIGINT AUTO_INCREMENT PRIMARY KEY,"
                " tweet_id VARCHAR(64) NOT NULL,"
                " tweet_url VARCHAR(512) NOT NULL DEFAULT '',"
                " success TINYINT(1) NOT NULL DEFAULT 0,"
                " error TEXT NULL,"
                " liked_at DATETIME(3) NULL,"
                " created_at DATETIME(3) NULL,"
                " machine_id VARCHAR(64) NOT NULL DEFAULT '',"
                " account_id BIGINT NULL,"
                " account_username VARCHAR(128) NOT NULL DEFAULT '',"
                " UNIQUE KEY uk_tweet_machine_account (tweet_id, machine_id, account_id),"
                " INDEX idx_like_records_tweet (tweet_id),"
                " INDEX idx_like_records_machine (machine_id)"
                ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
            )
            # 老表补齐新增列
            for col, ddl in (
                ("machine_id", "VARCHAR(64) NOT NULL DEFAULT '' AFTER created_at"),
                ("account_id", "BIGINT NULL AFTER machine_id"),
                ("account_username", "VARCHAR(128) NOT NULL DEFAULT '' AFTER account_id"),
            ):
                cur.execute(
                    "SELECT COUNT(*) AS cnt FROM information_schema.COLUMNS "
                    "WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'like_records' "
                    "AND COLUMN_NAME = %s",
                    (col,),
                )
                if cur.fetchone()["cnt"] == 0:
                    cur.execute(
                        "ALTER TABLE like_records ADD COLUMN %s %s" % (col, ddl)
                    )
    except Exception as exc:
        logger.warning("failed to ensure like_records table: %s", exc)


def _ensure_like_jobs_table() -> None:
    """兜底创建 like_jobs（与 Go 侧 GORM 迁移后的结构兼容）。"""
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "CREATE TABLE IF NOT EXISTS like_jobs ("
                " id BIGINT AUTO_INCREMENT PRIMARY KEY,"
                " follow_target_id BIGINT NOT NULL DEFAULT 0,"
                " handle VARCHAR(128) NOT NULL DEFAULT '',"
                " tweet_id VARCHAR(64) NOT NULL,"
                " machine_id VARCHAR(64) NOT NULL DEFAULT '',"
                " tweet_url VARCHAR(512) NOT NULL DEFAULT '',"
                " content TEXT NULL,"
                " scheduled_at DATETIME(3) NOT NULL,"
                " status VARCHAR(16) NOT NULL DEFAULT 'pending',"
                " attempts INT NOT NULL DEFAULT 0,"
                " account_id BIGINT NULL,"
                " account_username VARCHAR(128) NOT NULL DEFAULT '',"
                " error TEXT NULL,"
                " claimed_at DATETIME(3) NULL,"
                " done_at DATETIME(3) NULL,"
                " created_at DATETIME(3) NULL,"
                " UNIQUE KEY uk_tweet_machine (tweet_id, machine_id),"
                " KEY idx_machine_status_time (machine_id, status, scheduled_at)"
                ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
            )
    except Exception as exc:
        logger.warning("failed to ensure like_jobs table: %s", exc)


# ---------------------------------------------------------------------------
# Activity inputs
# ---------------------------------------------------------------------------


@dataclass
class LikeInput:
    tweet_id: str = ""
    tweet_url: str = ""
    author: str = ""
    content: str = ""


@dataclass
class LikeBatchInput:
    targets: list[dict[str, str]]  # list of {tweet_id, tweet_url, author, content}
    delay_between: float = 3.0


# ---------------------------------------------------------------------------
# Activities: like
# ---------------------------------------------------------------------------


class LikeActivities:
    """Like tweets on X/Twitter and record results in MySQL."""

    @activity.defn(name="LikeActivities.like")
    @traced("LikeActivities.like")
    async def like(self, input: LikeInput) -> dict[str, Any]:
        target = LikeTarget(
            tweet_id=input.tweet_id,
            tweet_url=input.tweet_url,
            author=input.author,
            content=input.content,
        )
        account = _account_manager.pick()
        cookies = (
            _x_like.account_cookies(account)
            if account is not None
            else None
        )
        result = await _x_like.like(target, cookies=cookies)
        if account is not None:
            _account_manager.mark_used(account.id)
        _save_result(result, machine_id=_MACHINE_ID, account=account)
        return {
            "tweet_id": result.tweet_id,
            "tweet_url": result.tweet_url,
            "success": result.success,
            "error": result.error,
        }

    @activity.defn(name="LikeActivities.like_batch")
    @traced("LikeActivities.like_batch")
    async def like_batch(self, input: LikeBatchInput) -> dict[str, Any]:
        targets = [
            LikeTarget(
                tweet_id=t.get("tweet_id", ""),
                tweet_url=t.get("tweet_url", ""),
                author=t.get("author", ""),
                content=t.get("content", ""),
            )
            for t in input.targets
        ]
        # 随机打乱顺序，避免固定顺序操作
        random.shuffle(targets)

        results: list[LikeResult] = []
        last_account_id: int | None = None
        for i, target in enumerate(targets):
            # 随机挑账号，并避免与上一个相同
            account = _account_manager.pick(exclude_id=last_account_id)
            cookies = (
                _x_like.account_cookies(account)
                if account is not None
                else None
            )
            result = await _x_like.like(target, cookies=cookies)
            if account is not None:
                _account_manager.mark_used(account.id)
                last_account_id = account.id
            _save_result(result, machine_id=_MACHINE_ID, account=account)
            results.append(result)

            if i < len(targets) - 1:
                # 随机间隔（默认 30~180 秒），覆盖输入里的固定 delay
                await asyncio.sleep(_random_delay())

        return {
            "total": len(results),
            "success": sum(1 for r in results if r.success),
            "failed": sum(1 for r in results if not r.success),
            "results": [
                {
                    "tweet_id": r.tweet_id,
                    "tweet_url": r.tweet_url,
                    "success": r.success,
                    "error": r.error,
                }
                for r in results
            ],
        }


# ---------------------------------------------------------------------------
# Activities: subscribe & like
# ---------------------------------------------------------------------------


class FollowActivities:
    """订阅目标点赞分发：从数据库读取订阅目标的推文（定时抓取已入库，不再自行抓取 X 网页），
    为所有机器生成随机时间的点赞任务。

    点赞本身不再由 activity 直接执行——每台机器的后台执行器
    （_like_executor_loop）按 like_jobs.scheduled_at 到期后自己点赞。
    """

    @activity.defn(name="FollowActivities.dispatch_likes")
    @traced("FollowActivities.dispatch_likes")
    async def dispatch_likes(self, target_id: int | None = None) -> dict[str, Any]:
        """从 raw_items 读取订阅目标的最新推文，为每台机器生成随机时间的点赞任务（like_jobs）。

        Args:
            target_id: optional, 只分发指定目标；None 则分发所有启用目标。
        """
        return await self._dispatch(target_id)

    @activity.defn(name="FollowActivities.scan_and_like")
    @traced("FollowActivities.scan_and_like")
    async def scan_and_like(self, target_id: int | None = None) -> dict[str, Any]:
        """兼容旧名：行为与 dispatch_likes 完全一致。"""
        return await self._dispatch(target_id)

    def _load_dispatch_targets(self, target_id: int | None = None) -> list[dict[str, Any]]:
        """加载所有启用（或指定 id）的订阅目标——不再按本机过滤，
        由分发器为全部机器生成任务。"""
        conn = _get_mysql()
        # Go 侧 TargetID 为 uint，0 表示"扫描所有目标"（见 follow_like.go），
        # 因此 0 与 None 等价，不能当作具体 id 查询（否则 WHERE id = 0 查不到任何目标）
        if target_id:
            sql = "SELECT id, handle FROM follow_targets WHERE enabled = 1 AND id = %s"
            params: tuple = (target_id,)
        else:
            sql = "SELECT id, handle FROM follow_targets WHERE enabled = 1"
            params = ()
        try:
            with conn.cursor() as cur:
                cur.execute(sql, params)
                return [dict(r) for r in cur.fetchall()]
        except Exception as exc:
            logger.warning("failed to load follow targets: %s", exc)
            return []

    def _machine_pool(self) -> list[str]:
        """机器池：所有有启用账号的机器；没有账号时退回默认机器（空串）。"""
        conn = _get_mysql()
        try:
            with conn.cursor() as cur:
                cur.execute(
                    "SELECT DISTINCT machine_id FROM x_accounts WHERE enabled = 1"
                )
                machines = sorted({str(r["machine_id"] or "") for r in cur.fetchall()})
        except Exception as exc:
            logger.warning("failed to load machine pool: %s", exc)
            return [""]
        return machines or [""]

    def _mark_checked(self, target_id: int) -> None:
        conn = _get_mysql()
        try:
            with conn.cursor() as cur:
                cur.execute(
                    "UPDATE follow_targets SET last_checked_at = NOW() WHERE id = %s",
                    (target_id,),
                )
        except Exception as exc:
            logger.warning("failed to mark target checked: %s", exc)

    def _load_recent_tweets(self, handle: str) -> list[dict[str, Any]]:
        """从 raw_items 读取该订阅目标最近 N 天（like_tweet_max_age_days）内尚未点赞的推文。

        定时抓取任务已入库，不再自行抓取；返回 [{tweet_id, url, content}]，
        external_id 形如 x_<tweet_id>，去掉前缀。
        """
        cutoff = (
            datetime.now(UTC).replace(tzinfo=None)
            - timedelta(days=_LIKE_TWEET_MAX_AGE_DAYS)
        ).strftime("%Y-%m-%d %H:%M:%S")
        conn = _get_mysql()
        try:
            with conn.cursor() as cur:
                cur.execute(
                    "SELECT external_id, url, content FROM raw_items "
                    "WHERE source = 'x' AND keyword = %s AND liked = 0 "
                    "AND published_at >= %s "
                    "ORDER BY published_at DESC LIMIT %s",
                    (handle, cutoff, _PROFILE_SCAN_LIMIT),
                )
                rows = [dict(r) for r in cur.fetchall()]
        except Exception as exc:
            logger.warning("failed to load recent tweets for @%s: %s", handle, exc)
            return []
        out: list[dict[str, Any]] = []
        for r in rows:
            ext = str(r.get("external_id") or "")
            tweet_id = ext[2:] if ext.startswith("x_") else ext
            if not tweet_id:
                continue
            out.append({
                "tweet_id": tweet_id,
                "url": r.get("url") or "",
                "content": r.get("content") or "",
            })
        return out

    async def _dispatch(self, target_id: int | None = None) -> dict[str, Any]:
        targets = self._load_dispatch_targets(target_id)
        if not targets:
            return {"targets": 0, "scanned": 0, "jobs_created": 0}

        machines = self._machine_pool()
        window_seconds = _LIKE_WINDOW_HOURS * 3600
        now = datetime.now(UTC).replace(tzinfo=None)

        scanned = 0
        jobs_created = 0
        for target in targets:
            tweets = self._load_recent_tweets(target["handle"])
            if not tweets:
                # 数据库暂无可点赞推文也标记检查时间，避免每次 cron 重复扫
                self._mark_checked(target["id"])
                continue
            scanned += 1

            # 每条推文 × 每台机器一个任务，scheduled_at 在窗口内随机分布
            rows: list[tuple] = []
            for t in tweets:
                for m in machines:
                    sched = now + timedelta(seconds=random.uniform(0, window_seconds))
                    rows.append((
                        target["id"],
                        target["handle"],
                        t["tweet_id"],
                        m,
                        t.get("url", ""),
                        (t.get("content") or "")[:2000],
                        sched.strftime("%Y-%m-%d %H:%M:%S"),
                        now.strftime("%Y-%m-%d %H:%M:%S"),
                    ))

            conn = _get_mysql()
            try:
                with conn.cursor() as cur:
                    cur.executemany(
                        "INSERT IGNORE INTO like_jobs "
                        "(follow_target_id, handle, tweet_id, machine_id, tweet_url, "
                        " content, scheduled_at, created_at) "
                        "VALUES (%s, %s, %s, %s, %s, %s, %s, %s)",
                        rows,
                    )
                    jobs_created += cur.rowcount
            except Exception as exc:
                logger.warning("failed to insert like_jobs for @%s: %s", target["handle"], exc)
            finally:
                self._mark_checked(target["id"])

        return {"targets": len(targets), "scanned": scanned, "jobs_created": jobs_created}


# ---------------------------------------------------------------------------
# 点赞执行器：后台轮询本机到期的 like_jobs 任务
# ---------------------------------------------------------------------------


def _mark_raw_item_liked(tweet_id: str) -> None:
    """点赞成功后把 raw_items.liked 置 1，避免后续分发重复生成任务。"""
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "UPDATE raw_items SET liked = 1, liked_at = NOW() "
                "WHERE source = 'x' AND external_id = %s",
                (f"x_{tweet_id}",),
            )
    except Exception as exc:
        logger.warning("failed to mark raw item liked for %s: %s", tweet_id, exc)


def _finish_job(job_id: int, account: XAccount | None = None) -> None:
    """标记任务完成（成功或已由本机其他账号点过）。"""
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "UPDATE like_jobs SET status = 'done', account_id = %s, "
                " account_username = %s, done_at = NOW(), error = '' "
                "WHERE id = %s",
                (
                    account.id if account is not None else None,
                    (account.username or "")[:128] if account is not None else "",
                    job_id,
                ),
            )
    except Exception as exc:
        logger.warning("failed to finish like job %s: %s", job_id, exc)


def _complete_machine_job(tweet_id: str, account: XAccount) -> None:
    """会话内已点赞的补充推文：直接完成其本机 pending/claimed 任务，避免到点重复点赞。"""
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "UPDATE like_jobs SET status = 'done', account_id = %s, "
                " account_username = %s, done_at = NOW(), error = '' "
                "WHERE tweet_id = %s AND machine_id = %s AND status IN ('pending', 'claimed')",
                (
                    account.id,
                    (account.username or "")[:128],
                    tweet_id,
                    _MACHINE_ID,
                ),
            )
    except Exception as exc:
        logger.warning("failed to complete machine like job for %s: %s", tweet_id, exc)


def _requeue_job(job_id: int, error: str = "", attempts: int = 0) -> None:
    """任务失败后回退 pending 并顺延 15~60 分钟重试；超过上限标记 failed 并入库错误。"""
    new_attempts = (attempts or 0) + 1
    try:
        conn = _get_mysql()
        if new_attempts >= _MAX_JOB_ATTEMPTS:
            with conn.cursor() as cur:
                cur.execute(
                    "UPDATE like_jobs SET status = 'failed', attempts = %s, "
                    " error = %s, claimed_at = NULL WHERE id = %s",
                    (new_attempts, (error or "")[:2000], job_id),
                )
            _log_error(
                source="like_executor",
                message=f"like job {job_id} failed after {new_attempts} attempts: {error}",
            )
            return
        delay = random.uniform(900, 3600)
        sched = datetime.now(UTC).replace(tzinfo=None) + timedelta(seconds=delay)
        with conn.cursor() as cur:
            cur.execute(
                "UPDATE like_jobs SET status = 'pending', attempts = %s, "
                " error = %s, claimed_at = NULL, scheduled_at = %s WHERE id = %s",
                (new_attempts, (error or "")[:2000], sched.strftime("%Y-%m-%d %H:%M:%S"), job_id),
            )
    except Exception as exc:
        logger.warning("failed to requeue like job %s: %s", job_id, exc)


def _claim_due_job(machine_id: str) -> dict[str, Any] | None:
    """原子抢占一个本机到期的 pending 任务，返回任务行。"""
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "SELECT id FROM like_jobs WHERE machine_id = %s AND status = 'pending' "
                "AND scheduled_at <= NOW() ORDER BY scheduled_at LIMIT 1",
                (machine_id,),
            )
            row = cur.fetchone()
            if row is None:
                return None
            job_id = row["id"]
            # 条件更新：只有仍是 pending 才会抢占成功，避免并发重复执行
            cur.execute(
                "UPDATE like_jobs SET status = 'claimed', claimed_at = NOW() "
                "WHERE id = %s AND status = 'pending'",
                (job_id,),
            )
            if cur.rowcount == 0:
                return None
            cur.execute(
                "SELECT id, follow_target_id, handle, tweet_id, tweet_url, content, attempts "
                "FROM like_jobs WHERE id = %s",
                (job_id,),
            )
            return dict(cur.fetchone())
    except Exception as exc:
        logger.warning("failed to claim like job: %s", exc)
        return None


def _load_batch_extra_tweets(
    handle: str, account: XAccount, exclude_tweet_id: str, limit: int
) -> list[dict[str, str]]:
    """随机取同 handle 尚未点赞的推文作为会话补充（排除本账号已点过的），最多 limit 条。"""
    if limit <= 0 or not handle:
        return []
    cutoff = (
        datetime.now(UTC).replace(tzinfo=None)
        - timedelta(days=_LIKE_TWEET_MAX_AGE_DAYS)
    ).strftime("%Y-%m-%d %H:%M:%S")
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            cur.execute(
                "SELECT external_id, url, content FROM raw_items "
                "WHERE source = 'x' AND keyword = %s AND liked = 0 "
                "AND published_at >= %s AND external_id != %s "
                "AND external_id NOT IN ("
                "  SELECT CONCAT('x_', tweet_id) COLLATE utf8mb4_unicode_ci "
                "  FROM like_records "
                "  WHERE account_id = %s AND success = 1"
                ") "
                "ORDER BY RAND() LIMIT %s",
                (handle, cutoff, f"x_{exclude_tweet_id}", account.id, limit),
            )
            rows = [dict(r) for r in cur.fetchall()]
    except Exception as exc:
        logger.warning("failed to load batch extra tweets for @%s: %s", handle, exc)
        return []
    out: list[dict[str, str]] = []
    for r in rows:
        ext = str(r.get("external_id") or "")
        tweet_id = ext[2:] if ext.startswith("x_") else ext
        if not tweet_id:
            continue
        out.append({
            "tweet_id": tweet_id,
            "url": r.get("url") or "",
            "content": r.get("content") or "",
        })
    return out


@traced("pyreplay.execute_like_job")
async def _execute_like_job(row: dict[str, Any]) -> None:
    """执行一个已抢占的点赞任务：一次会话随机点赞 1~N 条。

    主推文（任务对应的那条）+ 随机补充同 handle 未点赞推文，模拟真人一次
    浏览点多个帖子；会话内用同一账号，间隔随机 30~180 秒。补充推文点赞
    成功后直接完成其本机任务，避免到点重复执行。
    """
    tweet_id = str(row["tweet_id"])
    machine_id = _MACHINE_ID
    handle = str(row.get("handle") or "")

    # 账号级去重：找出本机已点过该推文的账号并排除
    excluded: set[int] = set()
    try:
        conn = _get_mysql()
        with conn.cursor() as cur:
            # 安全网：本机已点过该推文（like_records 里已有成功记录）直接完成
            cur.execute(
                "SELECT id FROM like_records WHERE tweet_id = %s AND machine_id = %s "
                "AND success = 1 LIMIT 1",
                (tweet_id, machine_id),
            )
            if cur.fetchone() is not None:
                _finish_job(row["id"])
                return
            cur.execute(
                "SELECT account_id FROM like_records WHERE tweet_id = %s AND machine_id = %s "
                "AND account_id IS NOT NULL AND success = 1",
                (tweet_id, machine_id),
            )
            excluded = {int(r["account_id"]) for r in cur.fetchall()}
    except Exception as exc:
        logger.warning("failed to check like_records for %s: %s", tweet_id, exc)

    account = _account_manager.pick_exclude_many(excluded)
    if account is None:
        # 本机账号都用过了（或都在冷却）——顺延重试
        _requeue_job(row["id"], error="no eligible account on this machine", attempts=row["attempts"])
        return

    # 组装会话批次：主推文 + 随机 0~N-1 条同 handle 未点赞推文，打乱顺序更像真人
    batch: list[dict[str, Any]] = [{
        "tweet_id": tweet_id,
        "url": row["tweet_url"] or f"https://x.com/i/status/{tweet_id}",
        "content": row["content"] or "",
        "primary": True,
    }]
    extra_limit = random.randint(1, _LIKE_BATCH_MAX) - 1
    if extra_limit > 0 and handle:
        for t in _load_batch_extra_tweets(handle, account, tweet_id, extra_limit):
            batch.append({**t, "primary": False})
    random.shuffle(batch)

    logger.info(
        "like session: %d tweet(s) for @%s, account=%s",
        len(batch), handle or "-", account.username,
    )
    primary_success = False
    primary_error = ""
    for i, t in enumerate(batch):
        target = LikeTarget(
            tweet_id=t["tweet_id"],
            tweet_url=t["url"],
            content=t["content"],
        )
        result = await _x_like.like(target, cookies=_x_like.account_cookies(account))
        _save_result(result, machine_id=machine_id, account=account)
        if not result.success:
            # 单条点赞失败也写入 error_logs，board 的“错误日志”页（/errors）统一查看与处理
            _log_error(
                source="like_executor",
                message=(
                    f"like failed: tweet {t['tweet_id']} (@{handle or '-'}) "
                    f"account={account.username}: {result.error}"
                ),
                context=json.dumps({
                    "tweet_id": t["tweet_id"],
                    "tweet_url": t["tweet_url"],
                    "handle": handle,
                    "account_id": account.id,
                    "account_username": account.username,
                    "machine_id": machine_id,
                    "primary": t["primary"],
                }, ensure_ascii=False),
            )
        if result.success:
            _mark_raw_item_liked(t["tweet_id"])
            if t["primary"]:
                primary_success = True
                _finish_job(row["id"], account=account)
            else:
                _complete_machine_job(t["tweet_id"], account)
        elif t["primary"]:
            primary_error = result.error or ""
        logger.info(
            "  [%d/%d] tweet %s -> %s%s",
            i + 1, len(batch), t["tweet_id"],
            "OK" if result.success else "FAIL",
            "" if result.success else f": {result.error}",
        )
        if i < len(batch) - 1:
            await asyncio.sleep(_random_delay())

    _account_manager.mark_used(account.id)
    if not primary_success:
        _requeue_job(row["id"], error=primary_error, attempts=row["attempts"])


async def _process_one_due_job() -> bool:
    """抢占并执行一个到期任务；无任务时返回 False。"""
    row = _claim_due_job(_MACHINE_ID)
    if row is None:
        return False
    try:
        await _execute_like_job(row)
    except Exception as exc:
        logger.warning("like job %s crashed: %s", row["id"], exc)
        _requeue_job(row["id"], error=str(exc)[:2000], attempts=row["attempts"])
    return True


@traced("pyreplay.like_executor_loop")
async def _like_executor_loop() -> None:
    """后台轮询本机到期点赞任务（与 Temporal worker 并发运行）。

    处理完一个任务后随机等待 30~180 秒，避免固定节奏；
    无任务时每 60 秒检查一次。
    """
    logger.info("like executor loop started (machine_id=%s)", _MACHINE_ID or "default")
    while True:
        try:
            handled = await _process_one_due_job()
            if handled:
                await asyncio.sleep(_random_delay())
            else:
                await asyncio.sleep(60)
        except Exception as exc:
            logger.warning("like executor loop error: %s", exc)
            await asyncio.sleep(60)


# ---------------------------------------------------------------------------
# Worker entry point
# ---------------------------------------------------------------------------


async def main() -> None:
    logging.basicConfig(level=logging.INFO)
    init_tracing("pyreplay")
    _ensure_error_logs_table()
    _ensure_like_records_table()
    _ensure_like_jobs_table()
    host = os.environ["TEMPORAL_HOST_PORT"]
    namespace = os.environ.get("TEMPORAL_NAMESPACE", "default")
    task_queue = os.environ.get("TEMPORAL_PYTHON_TASK_QUEUE", "pyreplay-task-queue")

    client = await Client.connect(host, namespace=namespace)
    worker_instance = Worker(
        client,
        task_queue=task_queue,
        # TracingInterceptor 必须在最外层：接续 workflow 传来的 trace context
        interceptors=[TracingInterceptor(), ErrorLogInterceptor()],
        activities=[
            LikeActivities().like,
            LikeActivities().like_batch,
            FollowActivities().dispatch_likes,
            FollowActivities().scan_and_like,
        ],
    )
    logger.info("PyReplay worker started on task queue %s", task_queue)
    logger.info("MACHINE_ID=%s (点赞执行器只处理该机器下的 like_jobs)", _MACHINE_ID or "default")

    # 后台执行器与 Temporal worker 并发运行：各自独立事件循环任务
    executor_task = asyncio.create_task(_like_executor_loop())
    try:
        await worker_instance.run()
    finally:
        executor_task.cancel()
        try:
            await executor_task
        except asyncio.CancelledError:
            pass


if __name__ == "__main__":
    asyncio.run(main())
