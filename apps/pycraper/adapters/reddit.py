"""Reddit adapter: crawl4ai browser scraping (same as X adapter)."""

from __future__ import annotations

import json
from datetime import datetime
from typing import Any

from adapters.base import _load_json_config
from adapters.logging import get_logger
from adapters.models import RawItem
from adapters.reddit_scan import scan_reddit

logger = get_logger(__name__)


def _parse_attr_int(val: Any) -> int:
    if val is None or val == "":
        return 0
    try:
        return int(float(str(val)))
    except (ValueError, TypeError):
        return 0


class RedditAdapter:
    """Fetches subreddit posts via crawl4ai browser."""

    def __init__(self, headless: bool = True, debug: bool = False) -> None:
        config = _load_json_config("reddit.json")
        self.headless = headless
        self.debug = debug
        self.sort = config.get("sort", "hot")
        self.per_subreddit_limit = config.get("limit", 25)

    async def _fetch_subreddit(self, subreddit: str, sort: str, limit: int, debug: bool = False) -> list[dict]:
        logger.info("fetching r/%s %s (limit=%d)", subreddit, sort, limit)
        result = await scan_reddit(
            subreddit=subreddit, sort=sort, limit=limit,
            headless=self.headless, page_timeout=180000, max_retries=2,
            debug=debug,
        )
        if not result.get("success"):
            logger.warning("r/%s failed: %s", subreddit, result.get("error"))
            return []
        return result.get("posts", [])

    def _post_to_raw_item(self, post: dict, keyword: str) -> RawItem | None:
        import re as _re

        title = post.get("title") or post.get("post-title", "")
        permalink = post.get("permalink", "")
        author = post.get("author", "")
        url = post.get("url", "")
        domain = post.get("domain", "")
        score = _parse_attr_int(post.get("score", 0))
        num_comments = _parse_attr_int(post.get("comments", 0))

        subreddit = post.get("subreddit", "")
        if not subreddit and permalink:
            m = _re.match(r'/r/([^/]+)', permalink)
            if m:
                subreddit = m.group(1)

        content = (
            f"[r/{subreddit}] {title}\n"
            f"by u/{author} | 👍 {score} | 💬 {num_comments}"
        )
        if domain and domain != f"self.{subreddit}":
            content += f" | 🔗 {domain}"

        post_id = post.get("id", "")
        if not post_id:
            return None

        published_at = None
        raw_time = post.get("time", "")
        if raw_time:
            try:
                published_at = datetime.fromisoformat(str(raw_time).replace("Z", "+00:00"))
            except (ValueError, TypeError):
                pass

        reddit_url = f"https://www.reddit.com{permalink}" if permalink else url

        return RawItem(
            source="reddit",
            external_id=f"reddit_{post_id}",
            keyword=keyword,
            url=reddit_url,
            content=content,
            published_at=published_at,
            raw_json=json.dumps(post, ensure_ascii=False),
        )

    async def fetch(self, keyword: str, limit: int, mode: str = "") -> list[RawItem]:
        """Fetch Reddit posts.

        mode:
          "subreddit" (default) — keyword is the subreddit name
          "discover" — search subreddits by keyword, then fetch
        """
        if mode == "discover":
            subs = await self._discover_subreddits(keyword)
            if not subs:
                return []
        else:
            subs = [keyword]

        per_sub_limit = min(limit, 50) if limit > 0 else self.per_subreddit_limit
        items: list[RawItem] = []
        for sub in subs:
            posts = await self._fetch_subreddit(sub, self.sort, per_sub_limit, debug=self.debug)
            for post in posts:
                item = self._post_to_raw_item(post, sub)
                if item:
                    items.append(item)
            logger.info("r/%s: %d posts", sub, len(posts))

        return items

    async def _discover_subreddits(self, query: str) -> list[str]:
        logger.info("discovering subreddits for '%s'", query)
        try:
            import requests
            from urllib.parse import quote
            resp = requests.get(
                f"https://www.reddit.com/subreddits/search.json?q={quote(query)}&limit=10",
                headers={"User-Agent": "python:ai-workflow:v1.0 (by /u/computefluxbot)"},
                timeout=15,
            )
            if resp.status_code != 200:
                return []
            data = resp.json()
            children = data.get("data", {}).get("children", [])
            return [c["data"]["display_name"] for c in children if c.get("kind") == "t5"]
        except Exception as e:
            logger.warning("discovery failed: %s", e)
            return []
