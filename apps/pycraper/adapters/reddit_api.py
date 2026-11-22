"""Reddit API adapter: fetches via the official Reddit JSON API (OAuth2).

Requires ``REDDIT_CLIENT_ID`` and ``REDDIT_CLIENT_SECRET`` in environment.

Usage modes
-----------
- ``mode="subreddit"`` (default) — *keyword* is a subreddit name, fetch its posts.
- ``mode="discover"`` — search subreddits whose name/desc matches *keyword*,
  then fetch posts from each.
- ``mode="search"`` — search posts across all of Reddit by *keyword*.
"""

from __future__ import annotations

import json
import os
import threading
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any
from urllib.parse import quote

import requests
from requests.auth import HTTPBasicAuth

from adapters.base import _load_json_config
from adapters.logging import get_logger
from adapters.models import RawItem

# Auto-load .env so the adapter works standalone (tests, scripts, etc.)
try:
    from dotenv import load_dotenv
    load_dotenv(Path(__file__).resolve().parent.parent / ".env", override=False)
except ImportError:
    pass

logger = get_logger(__name__)

_USER_AGENT = "python:ai-workflow:v1.0 (by /u/computefluxbot)"


class RedditAPIAdapter:
    """Fetches Reddit content via the official Reddit JSON API.

    Uses OAuth2 **client credentials** grant.
    API credentials come from environment variables:

    - ``REDDIT_CLIENT_ID``
    - ``REDDIT_CLIENT_SECRET``

    Falls back to the existing ``config/reddit.json`` for default sort
    and per-subreddit limit so both adapters share the same config.
    """

    def __init__(self) -> None:
        # Re-read from env at init time in case caller overrode after module load
        self.client_id = os.environ.get("REDDIT_CLIENT_ID", "")
        self.client_secret = os.environ.get("REDDIT_CLIENT_SECRET", "")
        self._access_token: str | None = None
        self._token_expires_at: datetime | None = None
        # 并发 activity 可能同时发现 token 过期，用锁保证只刷新一次
        self._token_lock = threading.Lock()

        config = _load_json_config("reddit.json")
        self.sort = config.get("sort", "hot")
        self.per_subreddit_limit = config.get("limit", 25)

    # ------------------------------------------------------------------
    # OAuth2
    # ------------------------------------------------------------------

    def _get_access_token(self) -> str:
        """Obtain (or reuse) a cached OAuth2 access token, honoring expiry."""
        with self._token_lock:
            if self._access_token and self._token_expires_at and datetime.now(UTC) < self._token_expires_at:
                return self._access_token

            if not self.client_id or not self.client_secret:
                raise RuntimeError(
                    "Reddit API credentials not configured. "
                    "Set REDDIT_CLIENT_ID and REDDIT_CLIENT_SECRET in .env"
                )

            auth = HTTPBasicAuth(self.client_id, self.client_secret)
            data = {"grant_type": "client_credentials"}
            headers = {"User-Agent": _USER_AGENT}

            resp = requests.post(
                "https://www.reddit.com/api/v1/access_token",
                auth=auth,
                data=data,
                headers=headers,
                timeout=15,
            )
            resp.raise_for_status()
            token_data = resp.json()
            token: str = token_data["access_token"]
            # 解析 expires_in（秒）并提前 60s 过期，避免边界上用到刚失效的 token
            try:
                expires_in = int(token_data.get("expires_in", 3600))
            except (ValueError, TypeError):
                expires_in = 3600
            self._access_token = token
            self._token_expires_at = datetime.now(UTC) + timedelta(seconds=max(expires_in - 60, 60))
            logger.info("obtained new Reddit OAuth2 access token (expires in %ds)", expires_in)
            return token

    def _api_headers(self) -> dict[str, str]:
        return {
            "Authorization": f"Bearer {self._get_access_token()}",
            "User-Agent": _USER_AGENT,
        }

    def _request_json(self, url: str, params: dict[str, Any]) -> requests.Response:
        """GET with OAuth headers; on 401 clear the cached token and retry once."""
        for attempt in (1, 2):
            resp = requests.get(url, headers=self._api_headers(), params=params, timeout=15)
            if resp.status_code != 401 or attempt >= 2:
                return resp
            logger.warning("OAuth token rejected (401), refreshing token and retrying once")
            self._access_token = None
            self._token_expires_at = None
        return resp

    # ------------------------------------------------------------------
    # Low-level API calls
    # ------------------------------------------------------------------

    def _fetch_subreddit_posts(
        self, subreddit: str, sort: str, limit: int
    ) -> list[dict[str, Any]]:
        """Fetch ``limit`` posts from ``r/{subreddit}/{sort}``.

        Returns an empty list only for "no content" cases (404/403, empty
        result).  Network/rate-limit errors (timeout, 5xx, 429) are logged
        and re-raised so Temporal can retry the activity.
        """
        url = f"https://oauth.reddit.com/r/{quote(subreddit, safe='')}/{sort}.json"
        params = {"limit": min(limit, 100), "raw_json": 1}

        logger.info("API: fetching r/%s %s (limit=%d)", subreddit, sort, limit)
        try:
            resp = self._request_json(url, params)
            if resp.status_code == 404:
                logger.warning("r/%s not found (404), skipping", subreddit)
                return []
            if resp.status_code == 403:
                logger.warning("r/%s forbidden/private (403), skipping", subreddit)
                return []
            resp.raise_for_status()
        except requests.RequestException as e:
            # 超时/5xx/429 等可恢复错误：记录日志后抛出，让 Temporal 重试；
            # 只有 404/403/空内容返回空列表。
            logger.warning("r/%s request failed: %s", subreddit, e)
            raise

        data = resp.json()

        posts: list[dict[str, Any]] = []
        for child in data.get("data", {}).get("children", []):
            if child.get("kind") == "t3":
                posts.append(child["data"])
        return posts

    def _search_subreddits(self, query: str, limit: int = 10) -> list[str]:
        """Search subreddits whose name/description matches *query*."""
        logger.info("API: searching subreddits for '%s'", query)
        try:
            resp = self._request_json(
                "https://oauth.reddit.com/subreddits/search.json",
                {"q": query, "limit": limit, "raw_json": 1},
            )
            if resp.status_code == 404:
                logger.warning("subreddit search returned 404, skipping")
                return []
            resp.raise_for_status()
            data = resp.json()
            children = data.get("data", {}).get("children", [])
            return [
                c["data"]["display_name"]
                for c in children
                if c.get("kind") == "t5"
            ]
        except requests.RequestException as e:
            # 网络/限流错误：抛出让 Temporal 重试，而不是当作"无结果"
            logger.warning("API subreddit search failed: %s", e)
            raise

    def _search_posts(
        self, query: str, limit: int, sort: str = "relevance"
    ) -> list[dict[str, Any]]:
        """Search posts across all of Reddit by keyword."""
        logger.info("API: searching posts for '%s' (limit=%d)", query, limit)
        try:
            resp = self._request_json(
                "https://oauth.reddit.com/search.json",
                {
                    "q": query,
                    "limit": min(limit, 100),
                    "sort": sort,
                    "raw_json": 1,
                },
            )
            resp.raise_for_status()
            data = resp.json()
            posts: list[dict[str, Any]] = []
            for child in data.get("data", {}).get("children", []):
                if child.get("kind") == "t3":
                    posts.append(child["data"])
            return posts
        except requests.RequestException as e:
            # 网络/限流错误：抛出让 Temporal 重试，而不是当作"无结果"
            logger.warning("API post search failed: %s", e)
            raise

    # ------------------------------------------------------------------
    # Item conversion
    # ------------------------------------------------------------------

    def _post_to_raw_item(
        self, post: dict[str, Any], keyword: str
    ) -> RawItem | None:
        """Convert a single Reddit API post dict to a ``RawItem``."""
        post_id = post.get("id", "")
        if not post_id:
            return None

        title = post.get("title", "")
        author = post.get("author", "[deleted]")
        score = post.get("score", 0)
        num_comments = post.get("num_comments", 0)
        permalink = post.get("permalink", "")
        subreddit = post.get("subreddit", "")
        url = post.get("url", "")
        domain = post.get("domain", "")
        selftext = (post.get("selftext") or "").strip()
        over_18 = post.get("over_18", False)

        # Build summary line
        content = (
            f"[r/{subreddit}] {title}\n"
            f"by u/{author} | 👍 {score} | 💬 {num_comments}"
        )
        if domain and domain != f"self.{subreddit}":
            content += f" | 🔗 {domain}"
        if over_18:
            content += " | 🔞 NSFW"
        if selftext:
            preview = selftext[:300].replace("\n", " ").strip()
            content += f"\n\n{preview}"

        # Parse created_utc (epoch seconds)
        published_at: datetime | None = None
        created_utc = post.get("created_utc")
        if created_utc is not None:
            try:
                published_at = datetime.fromtimestamp(int(created_utc), tz=UTC)
            except (ValueError, OSError):
                pass

        reddit_url = (
            f"https://www.reddit.com{permalink}" if permalink else url
        )

        return RawItem(
            source="reddit",
            external_id=f"reddit_{post_id}",
            keyword=keyword,
            url=reddit_url,
            content=content,
            published_at=published_at,
            raw_json=json.dumps(post, ensure_ascii=False),
        )

    # ------------------------------------------------------------------
    # Public fetch interface
    # ------------------------------------------------------------------

    async def fetch(
        self, keyword: str, limit: int, mode: str = ""
    ) -> list[RawItem]:
        """Fetch Reddit posts via the official API.

        Args:
            keyword: subreddit name (mode=subreddit/discover) or search query
                     (mode=search).
            limit:   max total items to return.
            mode:    ``"subreddit"`` (default), ``"discover"``, or ``"search"``.

        Returns:
            List of ``RawItem`` with ``source="reddit"``.
        """
        # Credential check happens lazily inside _get_access_token()
        per_sub_limit = min(limit, 100) if limit > 0 else self.per_subreddit_limit

        if mode == "search":
            posts = self._search_posts(keyword, per_sub_limit)
            return [
                item
                for post in posts
                if (item := self._post_to_raw_item(post, keyword))
            ]

        if mode == "discover":
            subs = self._search_subreddits(keyword)
            if not subs:
                logger.info("no subreddits found for '%s'", keyword)
                return []
        else:
            subs = [keyword]

        items: list[RawItem] = []
        for sub in subs:
            posts = self._fetch_subreddit_posts(sub, self.sort, per_sub_limit)
            for post in posts:
                item = self._post_to_raw_item(post, sub)
                if item:
                    items.append(item)
            logger.info("r/%s: %d posts via API", sub, len(posts))

        return items
