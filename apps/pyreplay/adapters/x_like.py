"""X (Twitter) adapter for like/reply operations via Crawl4AI browser automation."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import random
import re
from pathlib import Path
from typing import Any

from crawl4ai import AsyncWebCrawler, BrowserConfig, CacheMode, CrawlerRunConfig

from adapters.models import LikeResult, LikeTarget, XAccount

logger = logging.getLogger(__name__)

# 从推文 URL 中提取纯数字 tweet ID
_X_TWEET_ID_RE = re.compile(r"/status/(\d+)")

# like 按钮选择器（多种兜底）
_LIKE_SELECTORS = [
    '[data-testid="like"]',
    '[aria-label*="Like"]',
    '[aria-label*="喜欢"]',
    'button[data-testid="like"]',
]

# 随机 UA，降低被 X 反爬识别为自动化浏览器的概率
_USER_AGENTS = [
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
]

# 在用户主页提取推文（id / url / 正文）的 JS
_EXTRACT_TWEETS_JS = r"""
(() => {
  const out = [];
  const seen = new Set();
  document.querySelectorAll('article[data-testid="tweet"]').forEach(a => {
    const link = a.querySelector('a[href*="/status/"]');
    if (!link) return;
    const m = (link.getAttribute('href') || '').match(/\/status\/(\d+)/);
    if (!m) return;
    if (seen.has(m[1])) return;
    seen.add(m[1]);
    const textEl = a.querySelector('[data-testid="tweetText"]');
    out.push({
      id: m[1],
      url: 'https://x.com' + (link.getAttribute('href') || '').split('?')[0],
      text: (textEl ? textEl.innerText : '').slice(0, 500),
    });
  });
  return out;
})()
"""


def _load_json_config(name: str) -> dict[str, Any]:
    config_path = Path(__file__).resolve().parent.parent / "config" / name
    if not config_path.exists():
        return {}
    try:
        return json.loads(config_path.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as exc:
        logger.warning("failed to load config %s: %s", config_path, exc)
        return {}


def cookies_from_tokens(auth_token: str, ct0: str) -> list[dict[str, Any]]:
    """Build X cookies from auth_token/ct0 (per-account)."""
    cookies: list[dict[str, Any]] = []
    if auth_token:
        cookies.append({
            "name": "auth_token",
            "value": auth_token,
            "domain": ".x.com",
            "path": "/",
            "secure": True,
            "httpOnly": True,
        })
    if ct0:
        cookies.append({
            "name": "ct0",
            "value": ct0,
            "domain": ".x.com",
            "path": "/",
            "secure": True,
            "httpOnly": False,
        })
    return cookies


class XLikeAdapter:
    """Like tweets on X/Twitter via browser automation.

    Uses the user's logged-in session (cookies) to perform the like action.
    Cookies can be passed per-account (multi-account rotation) or fall back to
    the X_AUTH_TOKEN / X_CT0 environment variables.
    """

    def __init__(self) -> None:
        config = _load_json_config("x.json")
        self.headless = config.get("headless", True)
        self.base_url = config.get("base_url", "https://x.com")
        # 配置类型防御：非法值回退默认，避免初始化即崩
        try:
            self.delay = float(config.get("delay_before_return_html", 2.0) or 2.0)
        except (TypeError, ValueError):
            self.delay = 2.0
        try:
            self.profile_scroll_steps = int(config.get("profile_scroll_steps", 5) or 5)
        except (TypeError, ValueError):
            self.profile_scroll_steps = 5

    def _browser_config(self, cookies: list[dict[str, Any]]) -> BrowserConfig:
        return BrowserConfig(
            headless=self.headless,
            verbose=False,
            cookies=cookies,
            enable_stealth=True,
            user_agent=random.choice(_USER_AGENTS),
            extra_args=[
                "--disable-blink-features=AutomationControlled",
                "--no-sandbox",
                "--disable-dev-shm-usage",
            ],
        )

    def _run_config(self) -> CrawlerRunConfig:
        # x.com 是流式页面（持续推送/轮询），networkidle 几乎永远不满足，会直接
        # 超时。改用 wait_until="load" + 等待推文 article 渲染（pycraper 已验证
        # 的配置模式），再配合 simulate_user/override_navigator 降低反爬拦截。
        return CrawlerRunConfig(
            cache_mode=CacheMode.BYPASS,
            wait_until="load",
            wait_for="css:article[data-testid='tweet']",
            page_timeout=60000,
            delay_before_return_html=self.delay,
            simulate_user=True,
            override_navigator=True,
        )

    async def like(
        self,
        target: LikeTarget,
        cookies: list[dict[str, Any]] | None = None,
    ) -> LikeResult:
        """Like a single tweet.

        Args:
            target: tweet to like.
            cookies: per-account cookies. If None, fall back to env
                (X_AUTH_TOKEN / X_CT0).
        """
        tweet_url = target.tweet_url
        if not tweet_url and target.tweet_id:
            tweet_url = f"https://x.com/i/status/{target.tweet_id}"

        if not tweet_url:
            return LikeResult(
                tweet_id=target.tweet_id,
                tweet_url="",
                success=False,
                error="no tweet_url or tweet_id provided",
            )

        logger.info("liking tweet: %s", tweet_url)

        # cookies 必须放进 BrowserConfig，在浏览器 context 创建时随 context 一起
        # 生效（Playwright context.add_cookies），而不是作为 arun() 的参数——
        # arun()/CrawlerRunConfig 都没有 cookies 字段，传进去会被静默丢弃。
        if cookies is None:
            cookies = self._build_cookies()
        browser_cfg = self._browser_config(cookies)
        run_cfg = self._run_config()

        # arun() 返回的 CrawlResult 里没有可交互的 page 对象（页面在返回前就已
        # 处理完毕）。要在页面加载完成后执行真实的点击操作，需要通过 crawl4ai
        # 的 hook 机制拿到存活的 Playwright page —— 这是 pycraper/adapters/x_scan.py
        # 里已经验证可用的模式。

        # 页面加载是瞬时性失败的高发点（x.com 流式页面 / 反爬拦截 / 网络抖动），
        # 失败后短暂随机等待再重试一次；点击失败（按钮未找到/已点赞）不重试。
        last_error = ""
        for attempt in range(2):
            if attempt > 0:
                logger.info("retrying like (attempt 2) for %s", tweet_url)
                await asyncio.sleep(random.uniform(5, 15))

            click_outcome: dict[str, Any] = {"liked": False, "reason": "hook did not run"}

            async def _on_before_retrieve_html(page, context=None, config=None):
                nonlocal click_outcome
                click_outcome = await self._click_like(page)

            try:
                async with AsyncWebCrawler(config=browser_cfg) as crawler:
                    strategy: Any = crawler.crawler_strategy
                    try:
                        strategy.set_hook("before_retrieve_html", _on_before_retrieve_html)
                    except Exception as exc:
                        logger.warning("failed to register like-click hook: %s", exc)

                    result = await crawler.arun(url=tweet_url, config=run_cfg)

                    if not result or not result.success:
                        last_error = (
                            f"page load failed: {result.error_message if result else 'no result'}"
                        )
                        continue  # 瞬时性加载失败 → 重试

                    liked = bool(click_outcome.get("liked"))
                    reason = click_outcome.get("reason", "")
                    if liked:
                        logger.info("like success (%s): %s", reason, tweet_url)
                    else:
                        logger.warning("like failed (%s): %s", reason, tweet_url)

                    return LikeResult(
                        tweet_id=target.tweet_id,
                        tweet_url=tweet_url,
                        success=liked,
                        error="" if liked else (reason or "like button not found"),
                    )

            except Exception as exc:
                logger.error("like failed for %s: %s", tweet_url, exc)
                last_error = str(exc)
                continue  # 异常（多为加载/网络层）→ 重试

        return LikeResult(
            tweet_id=target.tweet_id,
            tweet_url=tweet_url,
            success=False,
            error=last_error or "like failed after retries",
        )

    async def like_batch(
        self,
        targets: list[LikeTarget],
        delay_between: float = 3.0,
        cookies_provider=None,
    ) -> list[LikeResult]:
        """Like multiple tweets in sequence with delay between each.

        Args:
            targets: tweets to like.
            delay_between: fixed delay between operations (used when the
                caller does not provide a randomized delay range).
            cookies_provider: optional callable() -> list[dict] that returns
                per-account cookies (for account rotation). If None, each like
                falls back to env cookies.
        """
        results: list[LikeResult] = []
        for i, target in enumerate(targets):
            logger.info("batch like %d/%d: %s", i + 1, len(targets), target.tweet_id)
            cookies = cookies_provider() if cookies_provider else None
            result = await self.like(target, cookies=cookies)
            results.append(result)
            if i < len(targets) - 1:
                await asyncio.sleep(delay_between)
        return results

    async def scan_recent_tweets(
        self,
        handle: str,
        limit: int = 20,
        cookies: list[dict[str, Any]] | None = None,
    ) -> list[dict[str, str]]:
        """Scan a user's profile page and collect their recent tweets.

        Returns a list of {id, url, text}. Scrolls the profile page a few
        times to load more tweets (X virtual DOM).
        """
        profile_url = f"{self.base_url}/{handle.lstrip('@')}"
        if cookies is None:
            cookies = self._build_cookies()
        browser_cfg = self._browser_config(cookies)
        run_cfg = self._run_config()

        collected: list[dict[str, str]] = []

        async def _on_before_retrieve_html(page, context=None, config=None):
            nonlocal collected
            seen: set[str] = set()
            for _ in range(self.profile_scroll_steps):
                try:
                    items = await page.evaluate(_EXTRACT_TWEETS_JS)
                except Exception as exc:
                    logger.warning("scan extract error: %s", exc)
                    break
                for it in items:
                    tid = str(it.get("id", ""))
                    if tid and tid not in seen:
                        seen.add(tid)
                        collected.append({
                            "id": tid,
                            "url": it.get("url", ""),
                            "text": it.get("text", ""),
                        })
                if len(collected) >= limit:
                    break
                try:
                    await page.mouse.wheel(0, 2000)
                    await asyncio.sleep(1.5)
                except Exception:
                    break
            collected[:] = collected[:limit]

        try:
            async with AsyncWebCrawler(config=browser_cfg) as crawler:
                strategy: Any = crawler.crawler_strategy
                try:
                    strategy.set_hook("before_retrieve_html", _on_before_retrieve_html)
                except Exception as exc:
                    logger.warning("failed to register scan hook: %s", exc)

                result = await crawler.arun(url=profile_url, config=run_cfg)
                if not result or not result.success:
                    logger.warning(
                        "profile scan failed for %s: %s",
                        handle,
                        result.error_message if result else "no result",
                    )
            return collected
        except Exception as exc:
            logger.error("scan failed for %s: %s", handle, exc)
            return []

    async def _click_like(self, page: Any) -> dict[str, Any]:
        """Click the like button on a tweet page.

        Runs inside crawl4ai's ``before_retrieve_html`` hook, so *page* is a
        live Playwright Page for the already-loaded tweet URL.
        """
        try:
            for selector in _LIKE_SELECTORS:
                try:
                    btn = await page.query_selector(selector)
                    if not btn:
                        continue

                    aria_label = await btn.get_attribute("aria-label")
                    if aria_label and ("Unlike" in aria_label or "取消喜欢" in aria_label):
                        logger.debug("tweet already liked")
                        return {"liked": True, "reason": "already liked"}

                    await btn.click()
                    await asyncio.sleep(1.0)
                    return {"liked": True, "reason": "clicked"}
                except Exception:
                    continue

            # JS 兜底：直接调 X 的 like 按钮
            logger.debug("fallback: using JS like")
            clicked = await page.evaluate("""
                (() => {
                    const btn = document.querySelector('[data-testid="like"]');
                    if (btn) { btn.click(); return true; }
                    return false;
                })()
            """)
            if clicked:
                await asyncio.sleep(1.0)
                return {"liked": True, "reason": "clicked via JS fallback"}
            return {"liked": False, "reason": "like button not found or already liked"}

        except Exception as exc:
            logger.warning("click_like error: %s", exc)
            return {"liked": False, "reason": str(exc)}

    def _build_cookies(self) -> list[dict[str, Any]]:
        """Build cookies from environment variables (fallback)."""
        return cookies_from_tokens(
            os.environ.get("X_AUTH_TOKEN", ""),
            os.environ.get("X_CT0", ""),
        )

    @staticmethod
    def account_cookies(account: XAccount) -> list[dict[str, Any]]:
        """Build cookies from an XAccount model."""
        return cookies_from_tokens(account.auth_token, account.ct0)
