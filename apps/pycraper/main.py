"""Temporal Python worker for MCP fetch and publish activities."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import traceback
from dataclasses import dataclass, field
from datetime import UTC, datetime
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

from adapters.github import GitHubAdapter
from adapters.models import RawItem
from adapters.x import XAdapter
from adapters.xiaohongshu import XiaohongshuAdapter
from telemetry import current_trace_id, init_tracing, traced
from xscan_server import start_xscan_server

load_dotenv(Path(__file__).resolve().parent / ".env")

# ---------------------------------------------------------------------------
# Colored logging via stdout (journald preserves ANSI with -o cat)
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
    from datetime import UTC, datetime as _dt
    handler = logging.StreamHandler()
    handler.setFormatter(_ColoredFormatter(
        fmt=f"%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        datefmt="%H:%M:%S",
    ))
    logging.root.handlers = [handler]
    logging.root.setLevel(logging.INFO)


_setup_logging()


@dataclass
class FetchInput:
    source: str
    keyword: str
    limit: int = 10
    mode: str = ""
    workflow_id: str = ""


@dataclass
class PublishInput:
    source: str
    account_id: str
    content: str
    title: str = ""
    tags: list[str] = field(default_factory=list)
    images: list[str] = field(default_factory=list)


from agents.social_generator import generate_social_content

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Reddit adapter — auto-select between official API and browser scraper
# ---------------------------------------------------------------------------

_reddit_has_api_creds = bool(
    os.environ.get("REDDIT_CLIENT_ID") and os.environ.get("REDDIT_CLIENT_SECRET")
)

if _reddit_has_api_creds:
    from adapters.reddit_api import RedditAPIAdapter

    _reddit_adapter = RedditAPIAdapter()
    logger.info("Reddit: using official API adapter (reddit_api)")
else:
    from adapters.reddit import RedditAdapter

    _reddit_adapter = RedditAdapter()
    logger.warning(
        "Reddit: REDDIT_CLIENT_ID / REDDIT_CLIENT_SECRET not configured. "
        "Falling back to browser-based RedditAdapter (slower, less reliable). "
        "Set these env vars for faster official API access."
    )

_FETCHERS = {
    "x": XAdapter(),
    "xiaohongshu": XiaohongshuAdapter(),
    "github": GitHubAdapter(),
    "reddit": _reddit_adapter,
}

# Register explicit reddit_api key only when credentials are available
if _reddit_has_api_creds:
    from adapters.reddit_api import RedditAPIAdapter

    _FETCHERS["reddit_api"] = RedditAPIAdapter()

_PUBLISHERS = {
    "x": XAdapter(),
    "xiaohongshu": XiaohongshuAdapter(),
}

# ---------------------------------------------------------------------------
# MySQL connection (lazy, cached per worker process)
# ---------------------------------------------------------------------------

_mysql_conn: Any | None = None


def _get_mysql() -> Any:
    global _mysql_conn
    # 用局部变量承载连接：pyright 对模块级全局变量的收窄会在函数调用后被作废
    # （_connect_mysql() 可能修改全局），局部变量收窄可靠且行为不变。
    conn = _mysql_conn
    if conn is None:
        conn = _connect_mysql()
        _mysql_conn = conn
    try:
        # 长空闲后 MySQL 会断开连接（wait_timeout），ping 失败时自动重连
        conn.ping()
    except Exception:
        try:
            conn.close()
        except Exception:
            pass
        conn = _connect_mysql()
        _mysql_conn = conn
    return conn


def _connect_mysql() -> Any:
    return pymysql.connect(
        host=os.environ["MYSQL_HOST"],
        port=int(os.environ["MYSQL_PORT"]),
        user=os.environ["MYSQL_USER"],
        password=os.environ["MYSQL_PASSWORD"],
        database=os.environ["MYSQL_DATABASE"],
        charset="utf8mb4",
        autocommit=True,
        connect_timeout=5,
        read_timeout=30,
        write_timeout=30,
    )


# ---------------------------------------------------------------------------
# 统一错误日志：写入 error_logs 表，board 的“错误日志”页查看（system=pycraper）
# ---------------------------------------------------------------------------

_SYSTEM_NAME = "pycraper"

# 必需环境变量清单：缺失时启动即报错，避免运行中才 KeyError 崩溃
_REQUIRED_ENV = [
    "MYSQL_HOST", "MYSQL_PORT", "MYSQL_USER", "MYSQL_PASSWORD", "MYSQL_DATABASE",
    "TEMPORAL_HOST_PORT", "TEMPORAL_NAMESPACE", "TEMPORAL_PYTHON_TASK_QUEUE",
]


def _validate_env() -> None:
    missing = [k for k in _REQUIRED_ENV if not os.environ.get(k)]
    if missing:
        raise RuntimeError(
            "missing required env vars: %s "
            "(check apps/pycraper/.env or the systemd EnvironmentFile)"
            % ", ".join(missing)
        )


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
            except Exception as log_exc:
                # 错误日志写入失败不能吞掉原始异常，至少要留痕
                logger.warning("failed to write error log: %s", log_exc)
            raise


class ErrorLogInterceptor(Interceptor):
    def intercept_activity(
        self, next: ActivityInboundInterceptor
    ) -> ActivityInboundInterceptor:
        return _ErrorLogActivityInbound(next)


# ---------------------------------------------------------------------------
# Activities
# ---------------------------------------------------------------------------


class FetchActivities:
    """Fetch raw content from MCP sources and stage results in MySQL.

    All items are written to the ``fetch_cache`` table in one batch.
    The Go worker drains the table asynchronously so the activity
    returns a lightweight summary regardless of data volume.
    """

    @activity.defn(name="FetchActivities.fetch")
    @traced("FetchActivities.fetch")
    async def fetch(self, input: FetchInput) -> dict[str, Any]:
        logger = logging.getLogger(__name__)
        wfid = input.workflow_id or "default"

        sources = [input.source] if input.source else list(_FETCHERS.keys())
        all_items: list[RawItem] = []
        for source in sources:
            adapter = _FETCHERS.get(source)
            if adapter is None:
                raise ValueError(f"unknown source: {source}")
            logger.info(
                "fetch source=%s keyword=%s limit=%d",
                source, input.keyword, input.limit,
            )
            _heartbeat(f"fetching source={source}")
            try:
                items = await adapter.fetch(input.keyword, input.limit, input.mode)
            except Exception as exc:
                # 记录单个平台的抓取失败（抓取中断），再抛出让 Temporal 重试
                _log_error(
                    source=f"fetch:{source}",
                    workflow_id=wfid,
                    message=str(exc),
                    stack=traceback.format_exc(),
                    context=(
                        '{"keyword":"%s","limit":%d,"mode":"%s"}'
                        % (input.keyword or "", input.limit, input.mode or "")
                    ),
                )
                raise
            _heartbeat(f"fetched source={source} items={len(items)}")
            # X 冷却，避免反爬
            if source == "x":
                await asyncio.sleep(3)
            kw = input.keyword or ""
            for item in items:
                item.keyword = kw
            all_items.extend(items)

        # 所有来源都为空才算空结果（原逻辑在循环内提前 return，导致多源抓取被短路）
        if not all_items:
            logger.warning(
                "fetch %s: no items collected (sources=%s keyword=%r)",
                wfid, sources, input.keyword,
            )
            return {"total": 0, "inserted": 0}

        # Bulk-insert into MySQL fetch_cache.
        conn = _get_mysql()
        sql = (
            "INSERT IGNORE INTO fetch_caches "
            "(workflow_id, source, external_id, keyword, url, content, "
            " published_at, fetched_at, raw_json, created_at) "
            "VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s)"
        )
        now = datetime.now(UTC)
        rows: list[tuple] = []
        for item in all_items:
            rows.append((
                wfid,
                item.source,
                item.external_id or "",
                item.keyword or "",
                item.url or "",
                item.content or "",
                item.published_at.replace(tzinfo=None) if item.published_at else None,
                item.fetched_at.replace(tzinfo=None) if item.fetched_at else now.replace(tzinfo=None),
                item.raw_json or "{}",
                now.replace(tzinfo=None),
            ))

        with conn.cursor() as cur:
            cur.executemany(sql, rows)
        inserted = cur.rowcount

        return {"total": len(all_items), "inserted": inserted}


def _heartbeat(detail: str = "") -> None:
    """发送 Temporal activity 心跳；非 activity 上下文或失败时静默忽略。"""
    try:
        activity.heartbeat(detail)
    except Exception:
        pass


class PublishActivities:
    """Publish content to MCP platforms."""

    @activity.defn(name="PublishActivities.publish")
    @traced("PublishActivities.publish")
    async def publish(self, input: PublishInput) -> dict[str, Any]:
        adapter = _PUBLISHERS.get(input.source)
        if adapter is None:
            raise ValueError(f"unknown source: {input.source}")
        if isinstance(adapter, XiaohongshuAdapter):
            return await adapter.publish(
                input.account_id,
                input.content,
                title=input.title,
                tags=input.tags,
                images=input.images,
            )
        return await adapter.publish(input.account_id, input.content)


@activity.defn(name="SocialGenerateActivity.generate")
@traced("social_generate")
async def social_generate(input: dict) -> dict:
    """Generate social media posts from recent ranked content."""
    from agents.social_generator import SocialGenerateInput
    si = SocialGenerateInput(
        hours=input.get("hours", 24),
        count=input.get("count", 5),
        source=input.get("source", "x"),
    )
    return await generate_social_content(si)


async def main() -> None:
    logging.basicConfig(level=logging.INFO)
    init_tracing("pycraper")
    _validate_env()
    _ensure_error_logs_table()
    host = os.environ["TEMPORAL_HOST_PORT"]
    namespace = os.environ["TEMPORAL_NAMESPACE"]
    task_queue = os.environ["TEMPORAL_PYTHON_TASK_QUEUE"]

    client = await Client.connect(host, namespace=namespace)
    worker_instance = Worker(
        client,
        task_queue=task_queue,
        # TracingInterceptor 必须在最外层：接续 workflow 传来的 trace context
        interceptors=[TracingInterceptor(), ErrorLogInterceptor()],
        activities=[
            FetchActivities().fetch,
            PublishActivities().publish,
            social_generate,
        ],
    )
    logging.info("Python MCP worker started on task queue %s", task_queue)
    # x-scan service 端点（S0 部署统一后 workflow-api 容器内无 docker CLI，
    # cli 型的 docker exec 方式不可用，改为 service 型：POST :9504/invoke）
    xscan_port = int(os.environ.get("XSCAN_HTTP_PORT", "9504"))
    server_task = asyncio.create_task(start_xscan_server(xscan_port))
    try:
        await worker_instance.run()
    finally:
        server_task.cancel()


if __name__ == "__main__":
    asyncio.run(main())
