"""X (Twitter) adapter: MCP server or Crawl4AI scraper."""

from __future__ import annotations

import asyncio
import json
import os
import random
import re
from datetime import datetime
from pathlib import Path
from typing import Any

import adapters.x_scan as x_scan
from adapters.base import _load_json_config
from adapters.logging import get_logger
from adapters.mcp_client import MCPClient, split_command
from adapters.models import RawItem

logger = get_logger(__name__)

# 从推文 URL 中提取纯数字 tweet ID（例：x.com/user/status/2076292969141797163 → 2076292969141797163）
_X_TWEET_ID_RE = re.compile(r"/status/(\d+)")


def _parse_engagement_int(val: Any) -> int:
    """容错解析互动数：None/"" /"1.2K"/"1,234"/非法值均安全返回 int。"""
    if val is None or val == "":
        return 0
    s = str(val).strip().lower().replace(",", "")
    if s.endswith("k"):
        try:
            return int(float(s[:-1]) * 1000)
        except (ValueError, TypeError):
            return 0
    if s.endswith("m"):
        try:
            return int(float(s[:-1]) * 1000000)
        except (ValueError, TypeError):
            return 0
    try:
        return int(float(s))
    except (ValueError, TypeError):
        return 0


def _parse_datetime(value: str | None) -> datetime | None:
    """Parse an ISO-8601 datetime string, returning None on failure."""
    if not value:
        return None
    value = value.strip()
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(value)
    except ValueError:
        return None


class XAdapter:
    """Fetches tweets and publishes content via Crawl4AI scraper or MCP.

    Configuration is loaded from ``config/x.json``.
    """

    def __init__(self, headless: bool = True) -> None:
        config = _load_json_config("x.json")

        self.headless = headless
        self.command = config.get("mcp_x_command", "echo")
        self.fetch_tool = config.get("mcp_x_fetch_tool", "search_tweets")
        self.publish_tool = config.get("mcp_x_publish_tool", "post_tweet")
        self.use_scraper = config.get("use_scraper", True)
        self.config = config

    # ------------------------------------------------------------------
    # Fetch
    # ------------------------------------------------------------------

    async def fetch(
        self, keyword: str, limit: int, mode: str = "keyword"
    ) -> list[RawItem]:
        if self.use_scraper:
            return await self._fetch_scraper(keyword, limit, mode)
        if self.command == "echo":
            logger.debug("X command is 'echo', returning empty")
            return []

        logger.info("fetch X via MCP keyword=%r limit=%d mode=%r", keyword, limit, mode)
        executable, args = split_command(self.command)
        async with MCPClient(executable, args=args) as client:
            result = await client.call_tool(
                self.fetch_tool, {"query": keyword, "limit": limit, "mode": mode}
            )

        if result is None:
            return []
        if isinstance(result, list):
            return [RawItem.from_payload("x", item) for item in result]
        if isinstance(result, dict):
            return [RawItem.from_payload("x", result)]
        return [RawItem(source="x", content=str(result))]

    async def _fetch_scraper(
        self, keyword: str, limit: int, mode: str
    ) -> list[RawItem]:
        """Fetch tweets via the Crawl4AI browser scraper with retry + backoff."""
        base_dir = Path(__file__).resolve().parent.parent

        storage = self.config.get("storage_state_path")
        if storage:
            storage = str(base_dir / storage)

        # Proxy: prefer env var PROXY_LIST over config file (env wins for ops flexibility)
        proxy = os.environ.get("PROXY_LIST") or self.config.get("proxy_list") or None
        headless = self.headless
        delay = float(self.config.get("delay_before_return_html", 2.0))
        base_pause = int(self.config.get("scroll_base_pause_ms", 2000))
        reading_pause = int(self.config.get("scroll_reading_pause_ms", 4000))
        base_url = self.config.get("base_url", "https://x.com/search")
        target_url: str | None = None

        if mode == "user":
            target_url = f"https://x.com/{keyword.lstrip('@')}"

        # 动态计算超时: 2 分钟基础 + 每 10 条额外 30 秒
        page_timeout_ms = 120000 + int(limit / 10) * 30000

        # 导航超时与推文等待超时分离：
        # page.goto 的超时即 crawl4ai 的 page_timeout，也是每个代理/出口的单次导航上限。
        # 若沿用 page_timeout_ms（可达 210s+），死代理/被限流出口会各自挂满整个时长才轮换，
        # 导致 "All proxies failed" 前整体卡死数分钟。这里把导航压到 45s 让坏代理快速失败轮换，
        # 同时把完整动态预算通过 wait_for_timeout 保留给推文选择器的显式等待。
        nav_timeout_ms = 45000

        # Retry with exponential backoff: handles transient proxy failures,
        # connection resets, and X rate-limiting. Max 5 attempts (initial + 4 retries).
        max_attempts = 5
        last_error = ""
        for attempt in range(1, max_attempts + 1):
            if attempt > 1:
                # Exponential backoff capped at 30s (+ jitter): connection resets
                # are transient, so we retry faster than a pure exponential curve.
                backoff = min(30, 10 * (2 ** (attempt - 2))) + random.uniform(0, 5)
                logger.info(
                    "X scraper retry %d/%d after %.1fs backoff (keyword=%r)",
                    attempt, max_attempts, backoff, keyword,
                )
                await asyncio.sleep(backoff)

            # Always keep the proxy pool: the pool now ends with DIRECT as a
            # fallback, so a blocked direct egress no longer short-circuits it.
            attempt_proxy = proxy

            logger.info(
                "scraping X keyword=%r limit=%d mode=%r nav_timeout=%ds wait_timeout=%ds attempt=%d/%d",
                keyword, limit, mode, nav_timeout_ms // 1000, page_timeout_ms // 1000, attempt, max_attempts,
            )
            result = await x_scan.scan_x(
                keywords=[keyword],
                storage_state_path=storage or None,
                auth_token=os.environ.get("X_AUTH_TOKEN", ""),
                ct0=os.environ.get("X_CT0", ""),
                proxy_arg=attempt_proxy,
                headless=headless,
                output_path=None,
                base_url=base_url,
                url=target_url,
                scroll_target_count=limit,
                delay_before_return_html=delay,
                scroll_base_pause_ms=base_pause,
                scroll_reading_pause_ms=reading_pause,
                use_virtual_scroll=False,
                max_retries=2,
                page_timeout=nav_timeout_ms,
                wait_for_timeout=page_timeout_ms,
                wait_until="domcontentloaded",
            )

            if result.get("success"):
                if result.get("error"):
                    logger.warning("X scraper completed with error field: %s", result.get("error"))
                break

            # Partial success: tweets collected despite crawl error
            tweets = result.get("tweets", [])
            if tweets:
                logger.warning(
                    "X scraper partial success: %d tweets collected (error: %s)",
                    len(tweets), result.get("error", ""),
                )
                break

            last_error = result.get("error") or "unknown scraper error"
            logger.warning(
                "X scraper attempt %d/%d failed: %s",
                attempt, max_attempts, last_error,
            )

        else:
            # All attempts exhausted
            raise RuntimeError(
                f"X scraper failed after {max_attempts} attempts: {last_error}"
            )

        items: list[RawItem] = []
        for tweet in result.get("tweets", []):
            if not isinstance(tweet, dict):
                continue
            text = str(tweet.get("text", ""))
            author = str(tweet.get("author", ""))
            likes = _parse_engagement_int(tweet.get("likes", 0))
            retweets = _parse_engagement_int(tweet.get("retweets", 0))
            views = _parse_engagement_int(tweet.get("views", 0))
            replies = _parse_engagement_int(tweet.get("replies", 0))
            content = (
                f"{author}: {text}\n"
                f"👍 {likes} | 🔄 {retweets} | 👁 {views} | 💬 {replies}"
            )
            url = str(tweet.get("tweet_url", ""))
            # 从 URL 中提取纯数字 tweet ID 作为去重键，比完整 URL 更稳定
            external_id = url
            if m := _X_TWEET_ID_RE.search(url):
                external_id = f"x_{m.group(1)}"
            published_at = _parse_datetime(tweet.get("posted_at"))
            items.append(
                RawItem(
                    source="x",
                    external_id=external_id,
                    url=url,
                    content=content,
                    published_at=published_at,
                    raw_json=json.dumps(tweet, ensure_ascii=False),
                )
            )

        logger.info("X scrape complete: %d tweets", len(items))
        logger.info("X scrape end ---------------------------------------------", )
        return items

    # ------------------------------------------------------------------
    # Publish
    # ------------------------------------------------------------------

    async def publish(self, account_id: str, content: str) -> dict[str, Any]:
        if self.command == "echo":
            # 之前这里返回 success=True 造成“假成功”：MCP X 服务未配置时发布被静默丢弃。
            # 改为显式报错，让上层 workflow 记录失败而不是假装发布成功。
            raise RuntimeError(
                "X MCP server not configured (mcp_x_command is 'echo'); "
                "set mcp_x_command in config/x.json to enable publishing"
            )

        logger.info("publish X account=%s", account_id)
        executable, args = split_command(self.command)
        async with MCPClient(executable, args=args) as client:
            return await client.call_tool(
                self.publish_tool, {"account_id": account_id, "content": content}
            )
