import argparse
import asyncio
import json
import os
import random
import re
import sys
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any
from urllib.parse import quote, urlparse

from crawl4ai import (
    AsyncWebCrawler,
    BrowserConfig,
    CacheMode,
    CrawlerRunConfig,
    JsonCssExtractionStrategy,
    VirtualScrollConfig,
)
from crawl4ai.async_configs import ProxyConfig

try:
    from dotenv import load_dotenv

    # Load .env from the pycraper root (override=False so real env still wins).
    load_dotenv(Path(__file__).resolve().parent.parent / ".env", override=False)
except ImportError:
    pass

try:
    from temporalio import activity
    _HAS_ACTIVITY = True
except ImportError:
    activity = None
    _HAS_ACTIVITY = False

USER_AGENTS = [
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36",
]

# 全局并发上限：同一进程最多同时运行 2 个 Chromium 实例（内存与反爬保护）。
# 注意：模块级 Semaphore 在 Python 3.12 下首次 await 时才绑定事件循环，
# 同一个 worker 进程内所有 activity 共享同一个 loop，因此并发限制是进程级的。
_X_CRAWLER_SEMAPHORE = asyncio.Semaphore(2)


@asynccontextmanager
async def _limited_web_crawler(browser_config: BrowserConfig):
    """AsyncWebCrawler 上下文，带进程级并发上限，不改变原有语义。"""
    async with _X_CRAWLER_SEMAPHORE:
        async with AsyncWebCrawler(config=browser_config) as crawler:
            yield crawler


def _heartbeat(detail: str = "") -> None:
    """发送 Temporal activity 心跳；非 activity 上下文或失败时静默忽略。"""
    # activity 可能为 None（ImportError 兜底）；显式判空让类型检查器收窄类型
    if not _HAS_ACTIVITY or activity is None:
        return
    try:
        activity.heartbeat(detail)
    except Exception:
        pass


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

DEFAULT_KEYWORDS = [
    "AI gateway",
    "RPC 隐私",
    "API 被封",
]

# Hardcoded CSS schema for X tweet extraction — no LLM needed.
# See https://docs.crawl4ai.com/advanced/virtual-scroll/ and the skill's
# extraction_pipeline.py for the JsonCssExtractionStrategy pattern.
X_TWEET_SCHEMA = {
    "name": "tweets",
    "baseSelector": "article[data-testid='tweet']",
    "fields": [
        {
            "name": "author",
            "selector": '[data-testid="User-Name"]',
            "type": "text",
        },
        {
            "name": "text",
            "selector": '[data-testid="tweetText"]',
            "type": "text",
        },
        {
            "name": "posted_at",
            "selector": "time",
            "type": "attribute",
            "attribute": "datetime",
        },
        {
            "name": "tweet_url",
            "selector": 'a[href*="/status/"]',
            "type": "attribute",
            "attribute": "href",
        },
    ],
}

X_MANUAL_SCROLL_JS = r"""
async (config) => {
    const { maxSteps, targetCount, basePauseMs, readingPauseMs, debug } = config;
    const collected = [];
    const seen = new Set();

    const log = (msg) => {
        if (debug) console.log("[scroll]", msg);
    };

    const SELECTORS = {
        real: {
            article: "article[data-testid='tweet']",
            author: '[data-testid="User-Name"]',
            text: '[data-testid="tweetText"]',
            time: "time",
            url: 'a[href*="/status/"]',
        },
        mock: {
            article: "article.mock-tweet",
            author: ".author",
            text: ".text",
            time: ".posted",
            url: null,
        },
    };

    const detectMode = () => {
        if (document.querySelector(SELECTORS.real.article)) return "real";
        if (document.querySelector(SELECTORS.mock.article)) return "mock";
        return null;
    };

    const isScrollable = (el) => {
        if (!el) return false;
        const style = window.getComputedStyle(el);
        const overflow = style.overflowY;
        return (overflow === "auto" || overflow === "scroll") &&
               (el.scrollHeight > el.clientHeight + 50);
    };

    const findScrollTarget = () => {
        // 1. Try primaryColumn itself or its first scrollable descendant.
        const col = document.querySelector('[data-testid="primaryColumn"]');
        if (col) {
            if (isScrollable(col)) return col;
            for (const child of col.querySelectorAll("*")) {
                if (isScrollable(child)) return child;
            }
        }
        // 2. Try the main element.
        const main = document.querySelector("main");
        if (isScrollable(main)) return main;
        // 3. Try the document root / body.
        if (isScrollable(document.documentElement)) return document.documentElement;
        if (isScrollable(document.body)) return document.body;
        // 4. Last resort: window.
        return window;
    };

    const scrollBy = (target, amount) => {
        if (target === window) {
            window.scrollBy({ top: amount, behavior: "smooth" });
        } else if (target && typeof target.scrollBy === "function") {
            target.scrollBy({ top: amount, behavior: "smooth" });
        }
    };

    const scrollTo = (target, top) => {
        if (target === window) {
            window.scrollTo({ top: top, behavior: "smooth" });
        } else if (target && typeof target.scrollTo === "function") {
            target.scrollTo({ top: top, behavior: "smooth" });
        }
    };

    const normalizeUrl = (href) => {
        if (!href) return "";
        if (href.startsWith("http")) return href;
        return "https://x.com" + (href.startsWith("/") ? href : "/" + href);
    };

    const makeKey = (author, text, posted) => {
        return (author + "|" + text + "|" + posted).replace(/\s+/g, "");
    };

    const extractVisible = (mode) => {
        const s = SELECTORS[mode];
        const articles = document.querySelectorAll(s.article);
        let newlyCollected = 0;
        let totalHeight = 0;
        for (const article of articles) {
            totalHeight += article.offsetHeight || 0;
            const authorEl = article.querySelector(s.author);
            const textEl = article.querySelector(s.text);
            const timeEl = article.querySelector(s.time);
            const urlEl = s.url ? article.querySelector(s.url) : null;
            const author = authorEl ? authorEl.innerText : "";
            const text = textEl ? textEl.innerText : "";
            const postedAt = timeEl
                ? (timeEl.getAttribute("datetime") || timeEl.innerText)
                : "";
            const tweetUrl = urlEl ? normalizeUrl(urlEl.getAttribute("href")) : "";
            // 提取互动指标（基于元素文本内容，不受浏览器语言影响）
            const parseAbbrNum = (raw) => {
                raw = raw.replace(/,/g, "");
                const suffix = raw.slice(-1).toUpperCase();
                if (suffix === "K") return Math.round(parseFloat(raw) * 1000);
                if (suffix === "M") return Math.round(parseFloat(raw) * 1000000);
                if (suffix === "B") return Math.round(parseFloat(raw) * 1000000000);
                // 中文万=1万, 亿=1亿
                if (raw.endsWith("万")) return Math.round(parseFloat(raw) * 10000);
                if (raw.endsWith("亿")) return Math.round(parseFloat(raw) * 100000000);
                return parseInt(raw, 10) || 0;
            };
            const getCount = (el) => {
                if (!el) return 0;
                // 优先从元素文本内容提取首位数字（如 <span>2.8万</span> → 28000）
                const text = (el.textContent || "").trim();
                const m = text.match(/^([\d.,]+(?:万|亿|[KMB])?)/i);
                if (m) return parseAbbrNum(m[1]);
                // 回退：从 aria-label 提取第一个数字（中英文通用）
                const label = el.getAttribute("aria-label") || "";
                const m2 = label.match(/([\d.,]+(?:万|亿|[KMB])?)/);
                if (m2) return parseAbbrNum(m2[1]);
                return 0;
            };
            // 优先在互动行容器内查找，避免匹配到内嵌引用推文
            const engagementRow = article.querySelector('div[role="group"]');
            const scope = engagementRow || article;
            const likesEl = scope.querySelector('[data-testid="like"]');
            const retweetsEl = scope.querySelector('[data-testid="retweet"]');
            const viewsEl = scope.querySelector('a[href*="/analytics"]');
            const repliesEl = scope.querySelector('[data-testid="reply"]');

            const likes = getCount(likesEl);
            const retweets = getCount(retweetsEl);
            const views = getCount(viewsEl);
            const replies = getCount(repliesEl);

            // 诊断：全部为 0 时输出缺失了哪些元素
            if (likes === 0 && retweets === 0 && views === 0 && replies === 0) {
                log("WARN: all engagement=0, el found: likes=" + !!likesEl +
                    " retweets=" + !!retweetsEl + " views=" + !!viewsEl +
                    " replies=" + !!repliesEl + " row=" + !!engagementRow);
            }

            const key = makeKey(author, text, postedAt);
            if (key && !seen.has(key)) {
                seen.add(key);
                collected.push({
                    author: author.trim(),
                    text: text.trim(),
                    posted_at: String(postedAt).trim(),
                    tweet_url: tweetUrl,
                    likes: likes,
                    retweets: retweets,
                    views: views,
                    replies: replies,
                });
                newlyCollected += 1;
            }
        }
        return { newlyCollected, articleCount: articles.length, totalHeight };
    };

    const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

    const dispatchMouseMove = () => {
        const x = Math.floor(Math.random() * window.innerWidth);
        const y = Math.floor(Math.random() * window.innerHeight);
        const event = new MouseEvent("mousemove", {
            clientX: x,
            clientY: y,
            bubbles: true,
        });
        document.dispatchEvent(event);
    };

    const mode = detectMode();
    if (!mode) {
        log("no tweet selectors matched, returning empty");
        return collected;
    }
    log("detected mode=" + mode);

    let target = findScrollTarget();
    log("scroll target=" + (target === window ? "window" : target.tagName + "." + target.className) +
        " scrollHeight=" + (target === window ? document.documentElement.scrollHeight : target.scrollHeight) +
        " clientHeight=" + (target === window ? document.documentElement.clientHeight : target.clientHeight));

    let consecutiveNoNew = 0;
    for (let step = 0; step < maxSteps; step++) {
        const { newlyCollected, articleCount, totalHeight } = extractVisible(mode);

        if (newlyCollected === 0) {
            consecutiveNoNew += 1;
            if (consecutiveNoNew >= 3) {
                if (target !== window) {
                    log("container scroll produced no new tweets, switching to window");
                    target = window;
                    consecutiveNoNew = 0;
                    continue;
                }
                log("stopping after " + step + " steps, no new tweets");
                break;
            }
        } else {
            consecutiveNoNew = 0;
        }

        if (targetCount > 0 && collected.length >= targetCount) {
            log("target count reached: " + collected.length);
            break;
        }

        if (articleCount > 0 && Math.random() < 0.3) {
            dispatchMouseMove();
        }

        const avgHeight = articleCount > 0 ? totalHeight / articleCount : window.innerHeight * 0.4;
        const scrollAmount = Math.round(avgHeight * (1 + Math.random()));

        log("step " + (step + 1) + "/" + maxSteps +
            " target=" + (target === window ? "window" : target.tagName) +
            " scroll=" + scrollAmount + "px" +
            " visible=" + articleCount +
            " new=" + newlyCollected +
            " total=" + collected.length);

        if (Math.random() < 0.05) {
            scrollTo(target, 0);
            await sleep(800 + Math.random() * 700);
        } else if (Math.random() < 0.15) {
            const rollback = Math.round(80 + Math.random() * 100);
            scrollBy(target, -rollback);
            await sleep(600 + Math.random() * 600);
            scrollBy(target, scrollAmount + rollback);
        } else {
            scrollBy(target, scrollAmount);
        }

        const pause = basePauseMs + Math.random() * 2000;
        await sleep(pause);

        if (Math.random() < 0.3) {
            await sleep(readingPauseMs + Math.random() * 5000);
            if (Math.random() < 0.5) dispatchMouseMove();
        }
    }

    const finalResult = extractVisible(mode);
    log("done: total collected=" + collected.length +
        " final_visible=" + finalResult.articleCount);
    return collected;
}
"""


def safe_load_json(path: str, label: str) -> tuple[Any | None, str]:
    """Load JSON from *path* and return (data, ""). On failure return (None, msg)."""
    p = Path(path)
    if not p.exists():
        return None, f"{label} file not found: {path}"
    try:
        with p.open(encoding="utf-8") as f:
            data = json.load(f)
        return data, ""
    except json.JSONDecodeError as e:
        return None, f"{label} JSON decode error in {path}: {e}"
    except (OSError, PermissionError) as e:
        return None, f"{label} cannot read {path}: {e}"
    except Exception as e:
        return None, f"{label} unexpected error loading {path}: {type(e).__name__}: {e}"


def load_storage_state(path: str):
    data, err = safe_load_json(path, "storage_state")
    if err:
        print(f"[WARN] {err}", file=sys.stderr)
        return None
    if isinstance(data, dict) and ("origins" in data or "storage" in data):
        return data
    print(
        "[WARN] storage_state file does not look like Playwright storage state",
        file=sys.stderr,
    )
    return None


def load_cookies(path: str):
    data, err = safe_load_json(path, "cookies")
    if err:
        print(f"[WARN] {err}", file=sys.stderr)
        return None
    if isinstance(data, list):
        return data
    print("[WARN] cookies file is not a JSON list", file=sys.stderr)
    return None


def _build_x_cookies(auth_token: str = "", ct0: str = "") -> list[dict[str, Any]]:
    """Build X cookies list from auth_token + ct0 values."""
    return [
        {
            "name": "auth_token",
            "value": auth_token,
            "domain": ".x.com",
            "path": "/",
            "secure": True,
            "httpOnly": True,
        },
        {
            "name": "ct0",
            "value": ct0,
            "domain": ".x.com",
            "path": "/",
            "secure": True,
            "httpOnly": False,
        },
    ]


def validate_inputs(keywords: list[str], base_url: str) -> str:
    """Return an error message if inputs are invalid; otherwise empty string."""
    if not isinstance(keywords, list) or not keywords:
        return "keywords must be a non-empty list"
    for k in keywords:
        if not isinstance(k, str) or not k.strip():
            return "keywords must contain non-empty strings"
    if not isinstance(base_url, str) or not base_url.strip():
        return "base_url must be a non-empty string"
    parsed = urlparse(base_url)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        return f"base_url must be a valid http:// or https:// URL (got {base_url})"
    return ""


def make_error_payload(
    url: str,
    keywords: list[str],
    error: str,
    error_stage: str,
    error_type: str = "",
    proxy: bool = False,
    cookies_used: bool = False,
    use_virtual_scroll: bool = False,
    elapsed_seconds: float = 0.0,
    manual_tweets_collected: int = 0,
    tweets: list[dict] | None = None,
    markdown_sample: str = "",
) -> dict:
    """Return a consistent payload dict for failure paths."""
    return {
        "url": url,
        "keywords": keywords,
        "proxy": proxy,
        "cookies_used": cookies_used,
        "use_virtual_scroll": use_virtual_scroll,
        "success": False,
        "error": error,
        "error_type": error_type,
        "error_stage": error_stage,
        "elapsed_seconds": elapsed_seconds,
        "manual_tweets_collected": manual_tweets_collected,
        "tweets_found": len(tweets) if tweets else 0,
        "tweets": tweets or [],
        "markdown_sample": markdown_sample,
    }


def save_output(payload: dict, output_path: str | None, _log) -> bool:
    """Write *payload* to *output_path*. Return True on success."""
    if not output_path:
        return False
    try:
        out_path = Path(output_path)
        out_path.parent.mkdir(parents=True, exist_ok=True)
        out_path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
        _log("saved output", str(output_path))
        return True
    except Exception as e:
        _log("save output failed", f"{type(e).__name__}: {e}")
        return False


def safe_set_hook(strategy: Any, hook_name: str, handler: Any, _log) -> None:
    """Attach a hook and log a warning if the API does not support it."""
    try:
        strategy.set_hook(hook_name, handler)
        _log("hook registered", hook_name)
    except Exception as e:
        _log("hook registration failed", f"{hook_name}: {type(e).__name__}: {e}")


def detect_x_error(html: str) -> str:
    """Return a non-empty error string if X's login/error wall is detected."""
    text = (html or "").lower()
    markers = [
        "sign in to x",
        "log in to x",
        "join x today",
        "this page is unavailable",
        "something went wrong",
        "login wall",
        "rate limit exceeded",
    ]
    for m in markers:
        if m in text:
            return "X login/error wall detected"
    return ""


def build_proxy_config(proxy_arg: str | None) -> list | None:
    """Convert a comma-separated proxy list into a Crawl4AI ProxyConfig list.

    Proxies are shuffled so repeated calls don't always lead with the same
    (possibly dead) proxy. DIRECT is kept last as a fallback so a blocked
    direct egress (net::ERR_CONNECTION_RESET) doesn't short-circuit the pool.
    If no proxies are configured, returns a single DIRECT entry (not None)
    so crawl4ai doesn't report "All proxies failed" on a direct-connection timeout.
    """
    if not proxy_arg:
        return [ProxyConfig.DIRECT]
    proxies = [p.strip() for p in proxy_arg.split(",") if p.strip()]
    if not proxies:
        return [ProxyConfig.DIRECT]
    random.shuffle(proxies)
    return [ProxyConfig(server=p) for p in proxies] + [ProxyConfig.DIRECT]


def build_browser_config(
    headless: bool = True,
    cookies: list | None = None,
    storage_state: dict | None = None,
    user_data_dir: str | None = None,
    use_persistent_context: bool = False,
) -> BrowserConfig:
    extra_args = [
        "--disable-blink-features=AutomationControlled",
        "--no-sandbox",
        "--disable-dev-shm-usage",
    ]
    kwargs: dict[str, Any] = {
        "headless": headless,
        "user_agent": random.choice(USER_AGENTS),
        "viewport_width": random.randint(1280, 1440),
        "viewport_height": random.randint(800, 900),
        "extra_args": extra_args,
        "cookies": cookies or [],
        "text_mode": False,
        "enable_stealth": True,
    }
    if storage_state is not None:
        kwargs["storage_state"] = storage_state
    if user_data_dir is not None:
        kwargs["user_data_dir"] = user_data_dir
    if use_persistent_context:
        kwargs["use_persistent_context"] = use_persistent_context
    return BrowserConfig(**kwargs)


def build_run_config(
    wait_for: str | None = None,
    wait_until: str = "domcontentloaded",
    page_timeout: int = 600000,
    wait_for_timeout: int | None = None,
    delay_before_return_html: float = 2.0,
    scroll_max_steps: int = 99999,
    use_virtual_scroll: bool = False,
    proxy_config: list | None = None,
    max_retries: int = 2,
    remove_overlays: bool = False,
) -> CrawlerRunConfig:
    """Build CrawlerRunConfig per the crawl4ai skill / docs recommendations.

    - By default a custom Playwright hook performs realistic 1-2 tweet scrolling
      and collects tweets as they enter the viewport (handles X's virtual DOM).
    - JsonCssExtractionStrategy (no LLM cost) with a hardcoded X tweet schema.
    - Anti-bot settings: simulate_user, override_navigator. remove_overlay_elements
      is opt-in (off by default) because it strips X's sticky containers and
      empties the article list before extraction runs.
    """
    if use_virtual_scroll:
        virtual_scroll = VirtualScrollConfig(
            container_selector='[data-testid="primaryColumn"]',
            scroll_count=int(scroll_max_steps),
            scroll_by="container_height",
            wait_after_scroll=1.5,
        )
    else:
        virtual_scroll = None

    kwargs: dict[str, Any] = {
        "cache_mode": CacheMode.BYPASS,
        "wait_until": wait_until,
        "page_timeout": page_timeout,
        "delay_before_return_html": delay_before_return_html,
        "extraction_strategy": JsonCssExtractionStrategy(X_TWEET_SCHEMA),
        "max_retries": max_retries,
        "simulate_user": True,
        "override_navigator": True,
        "remove_overlay_elements": remove_overlays,
    }
    if wait_for:
        kwargs["wait_for"] = wait_for
    if wait_for_timeout is not None:
        kwargs["wait_for_timeout"] = wait_for_timeout
    if proxy_config is not None:
        kwargs["proxy_config"] = proxy_config
    if virtual_scroll is not None:
        kwargs["virtual_scroll_config"] = virtual_scroll
    return CrawlerRunConfig(**kwargs)


async def scan_x(
    keywords: list[str],
    cookies_path: str | None = None,
    storage_state_path: str | None = None,
    auth_token: str = "",
    ct0: str = "",
    proxy_arg: str | None = None,
    headless: bool = True,
    output_path: str | None = None,
    base_url: str = "https://x.com/search",
    url: str | None = None,
    wait_until: str = "domcontentloaded",
    page_timeout: int = 600000,
    wait_for_timeout: int | None = None,
    delay_before_return_html: float = 2.0,
    scroll_max_steps: int = 60,
    scroll_target_count: int = 30,
    scroll_base_pause_ms: int = 2000,
    scroll_reading_pause_ms: int = 4000,
    use_virtual_scroll: bool = False,
    max_retries: int = 2,
    remove_overlays: bool = False,
    user_data_dir: str | None = None,
    use_persistent_context: bool = False,
    debug: bool = False,
) -> dict:
    # ---------- 1. Input validation (cheap, before starting browser) ----------
    validation_error = validate_inputs(keywords, base_url)
    if validation_error:
        payload = make_error_payload(
            url=url or base_url or "",
            keywords=keywords if isinstance(keywords, list) else [],
            error=validation_error,
            error_stage="validation",
            error_type="ValidationError",
        )
        save_output(
            payload,
            output_path,
            lambda stage, extra: print(f"[scan 0.00s] {stage}{(' ' + extra) if extra else ''}", file=sys.stderr),
        )
        return payload

    # Build the target URL. If *url* is given, use it directly; otherwise derive from
    # base_url + keywords for backward compatibility.
    if url:
        target_url = url
    elif "x.com" in base_url:
        target_url = f"{base_url}?q={quote(' OR '.join(keywords))}&f=top"
    else:
        # Nitter / xcancel style
        target_url = f"{base_url}?f=tweets&q={quote(' OR '.join(keywords))}"

    cookies = (
        _build_x_cookies(auth_token, ct0)
        if auth_token and ct0
        else (load_cookies(cookies_path) if cookies_path else None)
    )
    storage_state = load_storage_state(storage_state_path) if storage_state_path else None
    has_session = bool(cookies) or bool(storage_state)
    if not has_session and "x.com" in target_url:
        err = (
            "No X session credentials: set X_AUTH_TOKEN/X_CT0 env vars, "
            "config storage_state_path, or cookies_path. "
            "X search without login is blocked by the login wall."
        )
        print(f"[WARN] {err}", file=sys.stderr)
        payload = make_error_payload(
            url=target_url,
            keywords=keywords,
            error=err,
            error_stage="validation",
            error_type="MissingCredentials",
        )
        save_output(
            payload,
            output_path,
            lambda stage, extra: print(f"[scan] {stage}{(' ' + extra) if extra else ''}", file=sys.stderr),
        )
        return payload

    # Auto-pick a wait selector based on the target site.
    # Use a compound selector: tweet article OR login-wall indicator.
    # This prevents the 60s timeout when X shows a login wall (no tweets rendered).
    effective_wait_for: str | None = None
    if "mock_x" in target_url or "localhost" in target_url:
        effective_wait_for = "css:.mock-tweet"

    proxy_config = build_proxy_config(proxy_arg)
    browser_config = build_browser_config(
        headless=headless,
        cookies=cookies,
        storage_state=storage_state,
        user_data_dir=user_data_dir,
        use_persistent_context=use_persistent_context,
    )
    run_config = build_run_config(
        wait_for=effective_wait_for,
        wait_until=wait_until,
        page_timeout=page_timeout,
        wait_for_timeout=wait_for_timeout,
        delay_before_return_html=delay_before_return_html,
        scroll_max_steps=scroll_max_steps,
        use_virtual_scroll=use_virtual_scroll,
        proxy_config=proxy_config,
        max_retries=max_retries,
        remove_overlays=remove_overlays,
    )

    # Jitter before request to mimic human pacing.
    await asyncio.sleep(random.uniform(1.5, 4.0))

    t0 = time.monotonic()

    # Pre-check: if we have no session and target is x.com, fail fast
    # (already handled above in validation, but double-check for edge cases
    # where cookies file exists but is empty/invalid).
    if "x.com" in target_url and not has_session:
        err = "No valid X session: cookies/storage_state loaded but appear empty"
        _log("pre-check failed", err)
        payload = make_error_payload(
            url=target_url,
            keywords=keywords,
            error=err,
            error_stage="validation",
            error_type="MissingCredentials",
        )
        save_output(payload, output_path, _log)
        return payload

    def _log(stage: str, extra: str = ""):
        ts = time.monotonic() - t0
        print(
            f"[scan {ts:7.2f}s] {stage}{(' ' + extra) if extra else ''}",
            file=sys.stderr,
            flush=True,
        )

    if debug:
        _log("scan_x start", f"url={target_url}")

    manual_tweets: list[dict] = []
    manual_scroll_error: str = ""
    result: Any | None = None
    crawl_error: str = ""
    crawl_error_type: str = ""

    try:
        async with _limited_web_crawler(browser_config) as crawler:
            strategy: Any = crawler.crawler_strategy

            async def _before_retrieve_html(page, context=None, config=None):
                nonlocal manual_scroll_error
                if not use_virtual_scroll:
                    try:
                        if debug:
                            _log(
                                "manual_scroll",
                                f"starting max_steps={scroll_max_steps} target_count={scroll_target_count}",
                            )
                        eval_result = await page.evaluate(
                            X_MANUAL_SCROLL_JS,
                            {
                                "maxSteps": scroll_max_steps,
                                "targetCount": scroll_target_count,
                                "basePauseMs": scroll_base_pause_ms,
                                "readingPauseMs": scroll_reading_pause_ms,
                                "debug": debug,
                            },
                        )
                        if isinstance(eval_result, list):
                            manual_tweets.extend(eval_result)
                            if debug:
                                _log(
                                    "manual_scroll",
                                    f"collected={len(eval_result)} total_unique={len(manual_tweets)}",
                                )
                        else:
                            _log(
                                "manual_scroll",
                                f"unexpected return type={type(eval_result).__name__}",
                            )
                    except Exception as e:
                        manual_scroll_error = f"{type(e).__name__}: {str(e)[:200]}"
                        _log("manual_scroll failed", manual_scroll_error)

                try:
                    n = await page.evaluate("document.querySelectorAll(\"article[data-testid='tweet']\").length")
                    stage = "after virtual_scroll" if use_virtual_scroll else "after manual_scroll"
                    _log(stage, f"tweets in captured DOM={n}")
                except Exception as e:
                    _log(
                        "after scroll probe failed",
                        f"{type(e).__name__}: {str(e)[:200]}",
                    )

            safe_set_hook(strategy, "before_retrieve_html", _before_retrieve_html, _log)

            if debug:

                async def _on_page_context_created(page, context=None, config=None):
                    _log("page context ready", f"url={page.url}")

                    def _on_console(msg):
                        _log(f"browser console [{msg.type}]", msg.text[:300])

                    def _on_pageerror(err):
                        _log("browser pageerror", str(err)[:400])

                    page.on("console", _on_console)
                    page.on("pageerror", _on_pageerror)

                async def _before_goto(page, context=None, url=None, config=None):
                    _log("before_goto", f"-> {url}")

                async def _after_goto(page, context=None, url=None, config=None, **kw):
                    _log(
                        "after_goto",
                        f"at {page.url} title={(await page.title())[:50]!r}",
                    )

                safe_set_hook(strategy, "on_page_context_created", _on_page_context_created, _log)
                safe_set_hook(strategy, "before_goto", _before_goto, _log)
                safe_set_hook(strategy, "after_goto", _after_goto, _log)

            if debug:
                _log(
                    "arun() calling",
                    f"wait_for={effective_wait_for!r} wait_until={wait_until}",
                )

            # 滚动等待可能持续数分钟：后台周期性心跳保持 activity 存活
            async def _heartbeat_loop() -> None:
                while True:
                    _heartbeat("scan_x running")
                    await asyncio.sleep(15)

            hb_task = asyncio.create_task(_heartbeat_loop())
            try:
                result = await crawler.arun(url=target_url, config=run_config)
            except Exception as e:
                crawl_error_type = type(e).__name__
                crawl_error = str(e)[:500]
                _log("arun() raised", f"{crawl_error_type}: {crawl_error}")
            finally:
                hb_task.cancel()

            if debug and result is not None:
                _log(
                    "arun() returned",
                    f"success={result.success} status={getattr(result, 'status_code', '?')} "
                    f"html_len={len(result.html or '')} md_len={len(result.markdown or '')} "
                    f"extracted_len={len(result.extracted_content or '')}",
                )
    except Exception as e:
        # Catch-all for anything outside the inner arun() block (e.g., crawler context manager).
        crawl_error_type = type(e).__name__
        crawl_error = str(e)[:500]
        _log("crawler context failed", f"{crawl_error_type}: {crawl_error}")

    # ---------- Merge tweets from available sources ----------
    tweets: list[dict] = []
    seen_keys: set[str] = set()

    def _add_tweet(
        author: Any,
        text: Any,
        posted_at: Any,
        tweet_url: Any,
        likes: Any = 0,
        retweets: Any = 0,
        views: Any = 0,
        replies: Any = 0,
    ) -> None:
        text = re.sub(r"\s+", " ", (text or "")).strip()
        if not text:
            return
        author = re.sub(r"\s+", " ", (author or "")).strip()
        posted_at = re.sub(r"\s+", " ", (posted_at or "")).strip()
        tweet_url = (tweet_url or "").strip()
        key = re.sub(r"\s+", "", f"{author}|{text}|{posted_at}")
        if key not in seen_keys:
            seen_keys.add(key)
            tweets.append(
                {
                    "author": author,
                    "text": text,
                    "posted_at": posted_at,
                    "tweet_url": tweet_url,
                    "likes": _parse_engagement_int(likes),
                    "retweets": _parse_engagement_int(retweets),
                    "views": _parse_engagement_int(views),
                    "replies": _parse_engagement_int(replies),
                }
            )

    for item in manual_tweets:
        if isinstance(item, dict):
            _add_tweet(
                item.get("author"),
                item.get("text"),
                item.get("posted_at"),
                item.get("tweet_url"),
                item.get("likes", 0),
                item.get("retweets", 0),
                item.get("views", 0),
                item.get("replies", 0),
            )

    json_had_data = False
    if result is not None and getattr(result, "success", False) and getattr(result, "extracted_content", None):
        try:
            data = json.loads(result.extracted_content)
            candidates = data.get("tweets", []) if isinstance(data, dict) else data
            if isinstance(candidates, list) and candidates:
                json_had_data = True
                for item in candidates:
                    if isinstance(item, dict):
                        _add_tweet(
                            item.get("author"),
                            item.get("text"),
                            item.get("posted_at"),
                            item.get("tweet_url"),
                            item.get("likes", 0),
                            item.get("retweets", 0),
                            item.get("views", 0),
                            item.get("replies", 0),
                        )
        except Exception as e:
            _log("extraction parse failed", f"{type(e).__name__}: {e}")

    html = (result.html or "") if result is not None else ""
    if not json_had_data and html:
        # Fallback: scrape real-X articles OR mock-tweet class.
        try:
            from bs4 import BeautifulSoup

            soup = BeautifulSoup(html, "html.parser")
            selectors = [
                "article[data-testid='tweet']",
                "article.mock-tweet",
            ]
            for selector in selectors:
                for article in soup.select(selector):
                    user_name_el = article.select_one('[data-testid="User-Name"]')
                    text_el = article.select_one('[data-testid="tweetText"]')
                    time_el = article.select_one("time")
                    # Mock pages use simpler class-based selectors.
                    if not text_el:
                        text_el = article.select_one(".text")
                    if not user_name_el:
                        user_name_el = article.select_one(".author")
                    if not text_el:
                        continue
                    author = ""
                    if user_name_el:
                        handles = [
                            s.get_text(strip=True)
                            for s in user_name_el.find_all("span")
                            if s.get_text(strip=True).startswith("@")
                        ]
                        author = handles[0] if handles else user_name_el.get_text(strip=True).split("·")[0].strip()
                    posted_at = ""
                    if time_el:
                        posted_at = time_el.get("datetime") or time_el.get_text(strip=True)
                    else:
                        posted_el = article.select_one(".posted")
                        if posted_el:
                            posted_at = posted_el.get_text(strip=True)
                    _add_tweet(
                        author,
                        text_el.get_text("\n", strip=True),
                        posted_at,
                        "",
                    )
        except Exception as e:
            _log("HTML fallback parse failed", f"{type(e).__name__}: {e}")

    # ---------- Detect X login/error wall ----------
    x_error = detect_x_error(html) if "x.com" in target_url else ""
    # Also detect the case where the page loaded but no tweets appeared
    # (common when X serves a stale/empty shell to headless browsers).
    if not x_error and "x.com" in target_url and not tweets and not html:
        x_error = "X returned empty page (possible anti-bot block)"

    # ---------- Build final payload ----------
    elapsed = time.monotonic() - t0
    # 部分成功：手动滚动/提取已收集到推文时仍视为成功，error 字段附带失败原因，
    # 避免 crawl4ai arun() 失败但已有数据时整体丢弃。
    success = bool(tweets) or (bool(result and getattr(result, "success", False)) and not x_error)

    if not success and not crawl_error and not x_error and not manual_scroll_error:
        # Crawl returned but reported failure internally.
        crawl_error = (getattr(result, "error_message", None) or "") or "crawl returned without success"
        if not crawl_error_type:
            crawl_error_type = "CrawlError"

    # If manual scroll captured tweets but the crawl itself failed, we still report
    # the crawl error but keep the tweets collected so far.
    error_parts = [p for p in [x_error, crawl_error, manual_scroll_error] if p]
    error_message = "; ".join(error_parts)
    error_type = crawl_error_type or ("LoginWall" if x_error else "")
    error_stage = (
        "login_wall" if x_error else ("crawl" if crawl_error else ("manual_scroll" if manual_scroll_error else ""))
    )

    payload = {
        "url": target_url,
        "keywords": keywords,
        "proxy": bool(proxy_arg),
        "cookies_used": has_session,
        "use_virtual_scroll": use_virtual_scroll,
        "success": success,
        "error": error_message,
        "error_type": error_type,
        "error_stage": error_stage,
        "elapsed_seconds": round(elapsed, 2),
        "manual_tweets_collected": len(manual_tweets),
        "tweets_found": len(tweets),
        "tweets": tweets,
        "markdown_sample": ((result.markdown or "") if result is not None else "")[:2000],
    }

    save_output(payload, output_path, _log)
    return payload


def main():
    parser = argparse.ArgumentParser(description="Test scanning X posts with Crawl4AI")
    parser.add_argument("--keywords", nargs="+", default=DEFAULT_KEYWORDS, help="Search keywords")
    parser.add_argument(
        "--cookies",
        default="config/x_cookies.json",
        help="Path to cookies JSON (list of cookie dicts)",
    )
    parser.add_argument(
        "--storage-state",
        default=os.getenv("X_STORAGE_STATE"),
        help="Path to Playwright storage state JSON (recommended for X)",
    )
    parser.add_argument(
        "--proxy",
        default=os.getenv("PROXY_LIST"),
        help="Proxy or comma-separated list (will be tried in rotation with direct)",
    )
    parser.add_argument("--headful", action="store_true", help="Show browser window")
    parser.add_argument("--output", default="out/x_scan_results.json", help="Output JSON path")
    parser.add_argument(
        "--base-url",
        default="https://x.com/search",
        help="Search base URL (e.g. https://xcancel.com/search)",
    )
    parser.add_argument(
        "--wait-until",
        default="domcontentloaded",
        choices=["load", "domcontentloaded", "networkidle", "commit"],
        help="Playwright lifecycle event to wait for before extracting",
    )
    parser.add_argument(
        "--page-timeout",
        type=int,
        default=600000,
        help="Hard timeout for the crawl in ms (default 10 min)",
    )
    parser.add_argument(
        "--delay-before-return-html",
        type=float,
        default=2.0,
        help="Extra delay (seconds) after the auto-picked wait_for selector hits to let SPA hydrate",
    )
    parser.add_argument(
        "--scroll-max-steps",
        type=int,
        default=60,
        help="Maximum manual scroll steps (each 2-9s). Default 60 ≈ 5 minutes.",
    )
    parser.add_argument(
        "--scroll-target-count",
        type=int,
        default=30,
        help="Stop scrolling once this many unique tweets are collected (0 = no early stop).",
    )
    parser.add_argument(
        "--scroll-base-pause",
        type=int,
        default=2000,
        help="Base pause between scroll steps in milliseconds (default 2000).",
    )
    parser.add_argument(
        "--scroll-reading-pause",
        type=int,
        default=4000,
        help="Extra reading pause in milliseconds (30%% chance, default 4000).",
    )
    parser.add_argument(
        "--use-virtual-scroll",
        action="store_true",
        help="Use Crawl4AI's built-in VirtualScrollConfig instead of the realistic manual scroller.",
    )
    parser.add_argument("--max-retries", type=int, default=2, help="Crawl4AI max retries per proxy")
    parser.add_argument(
        "--remove-overlays",
        action="store_true",
        help=(
            "Enable crawl4ai remove_overlay_elements (off by default). "
            "WARNING: on X.com this removes sticky containers that wrap the "
            "tweet list, leaving the extraction strategy with an empty DOM."
        ),
    )
    parser.add_argument(
        "--no-browser-cache",
        action="store_true",
        help="Disable persistent browser profile (skip HTTP cache for images/CSS/JS)",
    )
    parser.add_argument(
        "--debug",
        action="store_true",
        help="Print detailed phase-by-phase timing logs and capture browser console / JS errors",
    )
    args = parser.parse_args()
    result = asyncio.run(
        scan_x(
            keywords=args.keywords,
            cookies_path=args.cookies,
            storage_state_path=args.storage_state,
            proxy_arg=args.proxy,
            headless=not args.headful,
            output_path=args.output,
            base_url=args.base_url,
            wait_until=args.wait_until,
            page_timeout=args.page_timeout,
            delay_before_return_html=args.delay_before_return_html,
            scroll_max_steps=args.scroll_max_steps,
            scroll_target_count=args.scroll_target_count,
            scroll_base_pause_ms=args.scroll_base_pause,
            scroll_reading_pause_ms=args.scroll_reading_pause,
            use_virtual_scroll=args.use_virtual_scroll,
            max_retries=args.max_retries,
            remove_overlays=args.remove_overlays,
            user_data_dir=(None if args.no_browser_cache else str(Path.home() / ".cache" / "x_scan_profile")),
            use_persistent_context=not args.no_browser_cache,
            debug=args.debug,
        )
    )

    print(f"Tweets found: {result['tweets_found']}")
    if not result["success"]:
        print(f"Error: {result['error']}", file=sys.stderr)
        if result.get("error_type"):
            print(f"Error type: {result['error_type']}", file=sys.stderr)
        if result.get("error_stage"):
            print(f"Error stage: {result['error_stage']}", file=sys.stderr)
        sys.exit(1)
    print(f"\nMarkdown sample:\n{result['markdown_sample'][:1000]}")
