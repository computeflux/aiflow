"""Reddit browser scraper using crawl4ai — simulates human browsing."""

from __future__ import annotations

import asyncio
import random
import re
import time
from pathlib import Path
from typing import Any
from urllib.parse import quote

from crawl4ai import AsyncWebCrawler, BrowserConfig, CacheMode, CrawlerRunConfig

try:
    from dotenv import load_dotenv
    load_dotenv(Path(__file__).resolve().parent.parent / ".env", override=False)
except ImportError:
    pass

USER_AGENTS = [
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
]

# ---------------------------------------------------------------------------
# JS: scroll the page progressively, collecting <shreddit-post> data.
# Data is collected in-browser before DOM nodes may be recycled.
# ---------------------------------------------------------------------------
SCROLL_JS_TPL = """
(async () => {
    const maxSteps = __STEPS__;
    const targetCount = __TARGET__;
    const debug = __DEBUG__;

    const log = (msg) => { if (debug) console.log("[reddit-scroll]", msg); };
    const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

    const collected = [];
    const seen = new Set();

    for (let step = 0; step < maxSteps; step++) {
        // Scroll by roughly one viewport
        const scrollBy = Math.round(window.innerHeight * (0.8 + Math.random() * 0.4));
        window.scrollBy({ top: scrollBy, behavior: "smooth" });

        // Wait for Reddit to load new posts
        await sleep(6000 + Math.random() * 2000);

        // Collect all <shreddit-post> currently in the DOM
        const posts = document.querySelectorAll("shreddit-post");
        let newlyCollected = 0;
        for (const post of posts) {
            const key = post.getAttribute("id") || post.getAttribute("permalink") || "";
            if (key && !seen.has(key)) {
                seen.add(key);
                const get = (name) => post.getAttribute(name) || "";
                const permalink = get("permalink");
                if (permalink) {
                    collected.push({
                        id: get("id"),
                        title: get("post-title"),
                        url: get("content-href"),
                        permalink: permalink,
                        author: get("author"),
                        subreddit: (get("subreddit-prefixed-name") || "").replace(/^r\\//, ""),
                        score: get("score"),
                        num_comments: get("comment-count"),
                        time: get("created-timestamp"),
                        domain: get("domain"),
                    });
                    newlyCollected++;
                }
            }
        }

        log("step " + (step + 1) + "/" + maxSteps +
            " scroll=" + scrollBy + "px collected=" + collected.length +
            " new=" + newlyCollected);

        if (collected.length >= targetCount) {
            log("target reached: " + collected.length);
            break;
        }
    }

    const finalVisible = document.querySelectorAll("shreddit-post").length;
    log("done: collected=" + collected.length + " visible=" + finalVisible);
    return collected.slice(0, targetCount);
})();
"""


def _build_scroll_js(steps: int, target: int, debug: bool) -> str:
    return (
        SCROLL_JS_TPL
        .replace("__STEPS__", str(steps))
        .replace("__TARGET__", str(target))
        .replace("__DEBUG__", "true" if debug else "false")
    )


def _parse_score(val: Any) -> int:
    s = str(val).strip().lower()
    s = s.replace(",", "")
    if s.endswith("k"):
        try:
            return int(float(s[:-1]) * 1000)
        except (ValueError, TypeError):
            pass
    try:
        return int(float(s))
    except (ValueError, TypeError):
        return 0


def _parse_attrs(attrs_str: str, permalink: str) -> dict | None:
    attrs = {}
    for m in re.finditer(r'(\w[\w-]*)="([^"]*)"', attrs_str):
        attrs[m.group(1)] = m.group(2)

    title = attrs.get("post-title", "")
    if not title:
        return None

    subreddit = attrs.get("subreddit-prefixed-name", "")
    if not subreddit:
        m = re.match(r'/r/([^/]+)', permalink)
        if m:
            subreddit = m.group(1)

    return {
        "id": attrs.get("id", ""),
        "title": title,
        "url": attrs.get("content-href", ""),
        "permalink": permalink,
        "author": attrs.get("author", ""),
        "subreddit": subreddit.lstrip("r/") if subreddit.startswith("r/") else subreddit,
        "score": _parse_score(attrs.get("score", "0")),
        "num_comments": _parse_score(attrs.get("comment-count", "0")),
        "time": attrs.get("created-timestamp", ""),
        "domain": attrs.get("domain", ""),
    }


async def scan_reddit(
    subreddit: str,
    sort: str = "hot",
    limit: int = 25,
    headless: bool = True,
    page_timeout: int = 300000,
    max_retries: int = 2,
    cookies_path: str | None = None,
    storage_state_path: str | None = None,
    proxy_arg: str | None = None,
    debug: bool = False,
) -> dict:
    """Scrape posts from a subreddit using crawl4ai.

    Simulates human scrolling to trigger Reddit's infinite-load before
    capturing the DOM.  Post data is collected **in-browser** during the
    scroll (before virtual DOM recycling removes old nodes).

    Args:
        subreddit: subreddit name (without r/)
        sort: hot / new / top
        limit: max posts to return
        headless: run browser in headless mode
        page_timeout: max page navigation timeout (ms)
        max_retries: retry count on failure
        cookies_path: path to cookies JSON
        storage_state_path: path to Playwright storage state
        proxy_arg: proxy list or URL
        debug: verbose logging

    Returns:
        {"success": bool, "posts": [...], "error": str}
    """
    if not subreddit or not str(subreddit).strip():
        return {"success": False, "posts": [], "error": "subreddit must be a non-empty string"}
    subreddit = str(subreddit).strip()
    target_url = f"https://www.reddit.com/r/{quote(subreddit, safe='')}/{sort}/"
    user_agent = random.choice(USER_AGENTS)

    scroll_steps = max(5, min(limit * 2, 50))
    scroll_target = max(limit, 10)
    scroll_js = _build_scroll_js(scroll_steps, scroll_target, debug)

    print(f"[scan_reddit] limit={limit} scroll_steps={scroll_steps} scroll_target={scroll_target}", flush=True)

    proxy_config = None
    if proxy_arg:
        from crawl4ai.async_configs import ProxyConfig

        proxies = proxy_arg.split("\n") if "\n" in proxy_arg else [proxy_arg]
        proxy_config = ProxyConfig(server=random.choice(proxies).strip()) if proxies else None

    browser_config = BrowserConfig(
        headless=headless,
        user_agent=user_agent,
        verbose=debug,
    )

    run_config = CrawlerRunConfig(
        cache_mode=CacheMode.BYPASS,
        wait_until="domcontentloaded",
        page_timeout=page_timeout,
        delay_before_return_html=3.0,
        simulate_user=True,
        max_retries=max_retries,
        wait_for="css:shreddit-post",
        wait_for_timeout=30000,
        flatten_shadow_dom=True,
    )
    if proxy_config:
        run_config.proxy_config = proxy_config

    t0 = time.monotonic()
    collected_posts: list[dict] = []

    try:
        async with AsyncWebCrawler(config=browser_config) as crawler:
            strategy = crawler.crawler_strategy

            async def _scroll_and_collect(page, context=None, config=None):
                nonlocal collected_posts

                def _on_console(msg):
                    if debug or "[reddit-scroll]" in (msg.text or ""):
                        print(f"[browser {msg.type}] {msg.text}", flush=True)
                page.on("console", _on_console)

                # Retry if navigation destroys the execution context
                result = None
                for attempt in range(2):
                    try:
                        result = await page.evaluate(scroll_js)
                        break
                    except Exception as nav_err:
                        err_msg = str(nav_err)
                        if "Execution context was destroyed" in err_msg or "navigation" in err_msg.lower():
                            print(f"[reddit] navigation detected, retrying (attempt {attempt+1})", flush=True)
                            await asyncio.sleep(5)
                            try:
                                await page.wait_for_load_state("networkidle", timeout=30000)
                            except Exception:
                                pass
                            continue
                        raise

                if isinstance(result, list):
                    valid = [p for p in result if isinstance(p, dict) and p.get("title")]
                    collected_posts.extend(valid)
                    print(f"[reddit] scroll collected {len(valid)} posts", flush=True)
                else:
                    print(f"[reddit] scroll result type={type(result).__name__}", flush=True)

            # 恢复滚动收集钩子：arun() 期间在页面内滚动并提前收集帖子，
            # 避免 Reddit 虚拟 DOM 回收节点导致丢数据。
            # 注：crawl4ai 类型桩（AsyncCrawlerStrategy）未声明 set_hook，pyright 会报 attr-defined；
            # 实际运行时 AsyncPlaywrightCrawlerStrategy 实例上存在该方法，仍包 try/except 兜底：
            # 若未来版本移除该 API，则退化为纯 HTML 解析（收集量减少但不中断）。
            try:
                strategy.set_hook("before_retrieve_html", _scroll_and_collect)  # type: ignore[attr-defined]
                print("[reddit] hook registered: before_retrieve_html", flush=True)
            except Exception as e:
                print(f"[reddit] WARNING: hook registration failed: {type(e).__name__}: {e}", flush=True)

            result = await crawler.arun(url=target_url, config=run_config)

        elapsed = time.monotonic() - t0

        if not result.success:
            return {"success": False, "posts": [], "error": result.error_message or "crawl failed"}

        posts = collected_posts[:limit]

        # HTML fallback for any posts still in the captured DOM
        if len(posts) < limit:
            html = result.html or ""
            post_re = re.compile(
                r'<shreddit-post\s+([^>]*?permalink="([^"]*)"[^>]*?)>'
                r'(.*?)'
                r'</shreddit-post>',
                re.DOTALL,
            )
            seen_permalinks = {p.get("permalink", "") for p in posts}
            for m in post_re.finditer(html):
                permalink_val = m.group(2)
                if permalink_val in seen_permalinks:
                    continue
                post = _parse_attrs(m.group(1), permalink_val)
                if post:
                    posts.append(post)
                if len(posts) >= limit:
                    break

        return {"success": True, "posts": posts, "elapsed": elapsed}

    except Exception as e:
        return {"success": False, "posts": [], "error": str(e)}
